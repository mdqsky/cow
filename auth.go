package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/cyfdecyf/bufio"
	"net"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	authRealm       = "cow proxy"
	authRawBodyTmpl = `<!DOCTYPE html>
<html>
	<head> <title>COW Proxy</title> </head>
	<body>
		<h1>407 Proxy authentication required</h1>
		<hr />
		Generated by <i>COW</i>
	</body>
</html>
`
)

type netAddr struct {
	ip   net.IP
	mask net.IPMask
}

type authUser struct {
	// user name is the key to auth.user, no need to store here
	passwd string
	ha1    string // used in request digest, initialized ondemand
	port   uint16 // 0 means any port
}

var auth struct {
	required bool

	user map[string]*authUser

	allowedClient []netAddr

	authed *TimeoutSet // cache authenticated users based on ip

	template *template.Template
}

func (au *authUser) initHA1(user string) {
	if au.ha1 == "" {
		au.ha1 = md5sum(user + ":" + authRealm + ":" + au.passwd)
	}
}

func parseUserPasswd(userPasswd string) (user string, au *authUser, err error) {
	arr := strings.Split(userPasswd, ":")
	n := len(arr)
	if n == 1 || n > 3 {
		err = errors.New("user password: " + userPasswd +
			" syntax wrong, should be username:password[:port]")
		return
	}
	user, passwd := arr[0], arr[1]
	if user == "" || passwd == "" {
		err = errors.New("user password " + userPasswd +
			" should not contain empty user name or password")
		return "", nil, err
	}
	var port int
	if n == 3 && arr[2] != "" {
		port, err = strconv.Atoi(arr[2])
		if err != nil || port <= 0 || port > 0xffff {
			err = errors.New("user password: " + userPasswd + " invalid port")
			return "", nil, err
		}
	}
	au = &authUser{passwd, "", uint16(port)}
	return user, au, nil
}

func parseAllowedClient(val string) {
	if val == "" {
		return
	}
	arr := strings.Split(val, ",")
	auth.allowedClient = make([]netAddr, len(arr))
	for i, v := range arr {
		s := strings.TrimSpace(v)
		ipAndMask := strings.Split(s, "/")
		if len(ipAndMask) > 2 {
			Fatal("allowedClient syntax error: client should be the form ip/nbitmask")
		}
		ip := net.ParseIP(ipAndMask[0])
		if ip == nil {
			Fatalf("allowedClient syntax error %s: ip address not valid\n", s)
		}
		var mask net.IPMask
		if len(ipAndMask) == 2 {
			nbit, err := strconv.Atoi(ipAndMask[1])
			if err != nil {
				Fatalf("allowedClient syntax error %s: %v\n", s, err)
			}
			if nbit > 32 {
				Fatal("allowedClient error: mask number should <= 32")
			}
			mask = NewNbitIPv4Mask(nbit)
		} else {
			mask = NewNbitIPv4Mask(32)
		}
		auth.allowedClient[i] = netAddr{ip.Mask(mask), mask}
	}
}

func addUserPasswd(val string) {
	if val == "" {
		return
	}
	user, au, err := parseUserPasswd(val)
	debug.Println("user:", user, "port:", au.port)
	if err != nil {
		Fatal(err)
	}
	if _, ok := auth.user[user]; ok {
		Fatal("duplicate user:", user)
	}
	auth.user[user] = au
}

func loadUserPasswdFile(file string) {
	if file == "" {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		Fatal("error opening user passwd fle:", err)
	}

	r := bufio.NewReader(f)
	s := bufio.NewScanner(r)
	for s.Scan() {
		addUserPasswd(s.Text())
	}
	f.Close()
}

func initAuth() {
	if config.UserPasswd != "" ||
		config.UserPasswdFile != "" ||
		config.AllowedClient != "" {
		auth.required = true
	} else {
		return
	}

	auth.user = make(map[string]*authUser)

	addUserPasswd(config.UserPasswd)
	loadUserPasswdFile(config.UserPasswdFile)
	parseAllowedClient(config.AllowedClient)

	auth.authed = NewTimeoutSet(time.Duration(config.AuthTimeout) * time.Hour)

	rawTemplate := "HTTP/1.1 407 Proxy Authentication Required\r\n" +
		"Proxy-Authenticate: Digest realm=\"" + authRealm + "\", nonce=\"{{.Nonce}}\", qop=\"auth\"\r\n" +
		"Content-Type: text/html\r\n" +
		"Cache-Control: no-cache\r\n" +
		"Content-Length: " + fmt.Sprintf("%d", len(authRawBodyTmpl)) + "\r\n\r\n" + authRawBodyTmpl
	var err error
	if auth.template, err = template.New("auth").Parse(rawTemplate); err != nil {
		Fatal("internal error generating auth template:", err)
	}
}

// Return err = nil if authentication succeed. nonce would be not empty if
// authentication is needed, and should be passed back on subsequent call.
func Authenticate(conn *clientConn, r *Request) (err error) {
	clientIP, _ := splitHostPort(conn.RemoteAddr().String())
	if auth.authed.has(clientIP) {
		debug.Printf("%s has already authed\n", clientIP)
		return
	}
	if authIP(clientIP) { // IP is allowed
		return
	}
	/*
		// No user specified
		if auth.user == "" {
			sendErrorPage(conn, "403 Forbidden", "Access forbidden",
				"You are not allowed to use the proxy.")
			return errShouldClose
		}
	*/
	err = authUserPasswd(conn, r)
	if err == nil {
		auth.authed.add(clientIP)
	}
	return
}

// authIP checks whether the client ip address matches one in allowedClient.
// It uses a sequential search.
func authIP(clientIP string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		panic("authIP should always get IP address")
	}

	for _, na := range auth.allowedClient {
		if ip.Mask(na.mask).Equal(na.ip) {
			debug.Printf("client ip %s allowed\n", clientIP)
			return true
		}
	}
	return false
}

func genNonce() string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%x", time.Now().Unix())
	return buf.String()
}

func calcRequestDigest(kv map[string]string, ha1, method string) string {
	// Refer to rfc2617 section 3.2.2.1 Request-Digest
	buf := bytes.NewBufferString(ha1)
	buf.WriteByte(':')
	buf.WriteString(kv["nonce"])
	buf.WriteByte(':')
	buf.WriteString(kv["nc"])
	buf.WriteByte(':')
	buf.WriteString(kv["cnonce"])
	buf.WriteByte(':')
	buf.WriteString("auth") // qop value
	buf.WriteByte(':')
	buf.WriteString(md5sum(method + ":" + kv["uri"]))

	return md5sum(buf.String())
}

func checkProxyAuthorization(conn *clientConn, r *Request) error {
	debug.Println("authorization:", r.ProxyAuthorization)
	arr := strings.SplitN(r.ProxyAuthorization, " ", 2)
	if len(arr) != 2 {
		errl.Println("auth: malformed ProxyAuthorization header:", r.ProxyAuthorization)
		return errBadRequest
	}
	if strings.ToLower(strings.TrimSpace(arr[0])) != "digest" {
		errl.Println("auth: client using unsupported authenticate method:", arr[0])
		return errBadRequest
	}
	authHeader := parseKeyValueList(arr[1])
	if len(authHeader) == 0 {
		errl.Println("auth: empty authorization list")
		return errBadRequest
	}
	nonceTime, err := strconv.ParseInt(authHeader["nonce"], 16, 64)
	if err != nil {
		return err
	}
	// If nonce time too early, reject. iOS will create a new connection to do
	// authenticate.
	if time.Now().Sub(time.Unix(nonceTime, 0)) > time.Minute {
		return errAuthRequired
	}

	user := authHeader["username"]
	au, ok := auth.user[user]
	if !ok {
		errl.Println("auth: no such user:", authHeader["username"])
		return errAuthRequired
	}

	if au.port != 0 {
		// check port
		_, portStr := splitHostPort(conn.LocalAddr().String())
		port, _ := strconv.Atoi(portStr)
		if uint16(port) != au.port {
			errl.Println("auth: user", user, "port not match")
			return errAuthRequired
		}
	}

	if authHeader["qop"] != "auth" {
		msg := "auth: qop wrong: " + authHeader["qop"]
		errl.Println(msg)
		return errors.New(msg)
	}

	response, ok := authHeader["response"]
	if !ok {
		msg := "auth: no request-digest"
		errl.Println(msg)
		return errors.New(msg)
	}

	au.initHA1(user)
	digest := calcRequestDigest(authHeader, au.ha1, r.Method)
	if response == digest {
		return nil
	}
	errl.Println("auth: digest not match, maybe password wrong")
	return errAuthRequired
}

func authUserPasswd(conn *clientConn, r *Request) (err error) {
	if r.ProxyAuthorization != "" {
		// client has sent authorization header
		err = checkProxyAuthorization(conn, r)
		if err == nil {
			return
		} else if err != errAuthRequired {
			sendErrorPage(conn, errCodeBadReq, "Bad authorization request", err.Error())
			return
		}
		// auth required to through the following
	}

	nonce := genNonce()
	data := struct {
		Nonce string
	}{
		nonce,
	}
	buf := new(bytes.Buffer)
	if err := auth.template.Execute(buf, data); err != nil {
		errl.Println("Error generating auth response:", err)
		return errInternal
	}
	if debug {
		debug.Printf("authorization response:\n%s", buf.String())
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		errl.Println("Sending auth response error:", err)
		return errShouldClose
	}
	return errAuthRequired
}
