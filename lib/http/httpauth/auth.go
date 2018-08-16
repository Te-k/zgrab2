package httpauth

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/zmap/zgrab2/lib/http"
	log "github.com/sirupsen/logrus"
)

type Authenticator interface {
	TryGetAuth(req *http.Request, resp *http.Response) string
}

// TODO: Make this contain state useful for constructing a next response (ie: nextnonce field)
// TODO: Session state ("-sess") could also be handy here, since it persists from one request to the next with a given host
// TODO: Similarly, maintaining nonce counter presents some interesting challenges. Maybe more state mapped from host makes sense
type authenticator struct {
	// Map from hosts to credential pointers. Shouldn't be accessed directly.
	creds map[string]*credential
}

type credential struct {
	Username, Password string
}

// TODO: Make sure that you can only specify one file? Maybe supporting multiple files makes sense.
func NewAuthenticator(credsFilename string, hostsToCreds map[string]string) (authenticator, error) {
	auther := authenticator{creds: make(map[string]*credential)}
	var err error
	// If a filename is given, record all {host, username:password} pairs it specifies.
	if credsFilename != "" {
		var fileHostsToCreds map[string]string
		// The only possible error here would result from os.Open on file.
		fileHostsToCreds, err = readCreds(credsFilename)
		populate(auther, fileHostsToCreds)
	}
	// If pairs are explicitly specified in a map[string]string, use them.
	// Override any pairs specified in a file with those specified in explicit map.
	if hostsToCreds != nil {
		populate(auther, hostsToCreds)
	}
	return auther, err
}

func readCreds(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		// TODO: Log with the correct logger and settle on a proper message for this. (ie: include filename)
		log.Warn("Couldn't open credentials file.")
		return nil, err
	}

	creds := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// TODO: Future: Add host-grouping syntax & special case for lines starting with a character meaningful therein
		parts := strings.Split(line, " ")
		host := parts[0]
		// Preserve any spaces in username:password by combining everything after
		// first space (particularly because spaces are legal in Basic Auth passwords)
		var userpass string
		if len(parts) > 1 {
			userpass = strings.Join(parts[1:], " ")
		}
		creds[host] = userpass
	}
	return creds, nil
}

// TODO: Future: Add support for IP addresses rather than only hostnames
// TODO: Future: Parse for wildcards & other options to specify a set of credentials for many hosts
// TODO: Should whether to use TLS be specified when setting up in the first place or for
	// each particular instance? Either way, it only needs to be passed in once. It's
	// really a matter of which makes more sense semantically.
// TODO: Future: Create a way to specify and lookup default credentials
// Subsequent calls to populate (only made from NewAuthenticator) will, if possible,
// overwrite the result of previous calls.
func populate(result authenticator, hostsToCreds map[string]string) {
	for host, userpass := range hostsToCreds {
		creds := strings.Split(userpass, ":")
		user := creds[0]
		// Preserve any colons in password by combining everything after first colon
		var pass string
		if len(creds) > 1 {
			pass = strings.Join(creds[1:], ":")
		}
		result.creds[host] = &credential{Username: user, Password: pass}
	}
}

// TODO: Improve names because "token" is inaccurate and "parts" imprecise. Same goes for "chunk".
func parseWwwAuth(header string) map[string]string {
	var inQuotes, escaped bool
	var tokens []string
	var chunk []rune
	for _, c := range header {
		if c == '=' && !inQuotes {
			tokens = append(tokens, string(chunk))
			chunk = chunk[:0]
			continue
		}
		// Toggles inQuotes when an unescaped quote is encountered
		if c == '"' && !escaped {
			inQuotes = !inQuotes
		}
		if c == '\\' {
			// Toggles escaped when consecutive backslashes are encountered
			escaped = !escaped
		} else {
			// Resets escaped to false once non-backslash is encountered
			escaped = false
		}
		chunk = append(chunk, c)
	}
	tokens = append(tokens, string(chunk))

	parameters := make(map[string]string)
	for i, token := range tokens[1:] {
		prevParts := strings.Split(tokens[i], " ")
		name := prevParts[len(prevParts)-1]
		var value string
		if token[:1] == `"` {
			parts := strings.Split(token, `"`)
			value = strings.Join(parts[:len(parts)-1], `"`) + `"`
		} else {
			parts := strings.Split(token, " ")
			if len(parts) > 1 {
				value = strings.Join(parts[:len(parts)-1], " ")
			}
			value = parts[0]
		}
		if value[len(value)-1:] == "," {
			value = value[:len(value)-1]
		}
		parameters[name] = value
	}

	return parameters
}

func unquote(s string) string {
	// A string can only be in quotes if it's at least two characters long.
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1:len(s)-1]
		}
		// TODO: Determine if any other escaping needs to be undone. I don't think so, since double-quotes are the relevant problem.
		s = strings.Replace(s, `\"`, `"`, -1)
	}
	return s
}

func keyedDigest(h func(string) string, secret, data string) (hash string) {
	return h(secret + ":" + data)
}

var algorithms map[string]func(string) string = map[string]func(string) string{
	"MD5": func(s string) string {
		return fmt.Sprintf("%x", md5.Sum([]byte(s)))
	},
	"SHA-256": func(s string) string {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
	},
	"SHA-512-256": func(s string) string {
		return fmt.Sprintf("%x", sha512.Sum512_256([]byte(s)))
	},
}

// TODO: Future: Replace with request.go's (identical) implementation of this if rolled into http package
func valueOrDefault(value, def string) string {
	if value != "" {
		return value
	}
	return def
}

func generateClientNonce() (string, error) {
	// Generates random 32-byte number
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// TODO: This function is quite long, but does essentially perform one task. Consider refactoring.
func getDigestAuth(creds *credential, req *http.Request, resp *http.Response) string {
	// Return quickly in the case that Authorization header can't be constructed
	if resp == nil || resp.Header == nil {
		return ""
	}

	// TODO: Add an option to work with Proxy-Authenticate header (maybe just take in headers overall?)
	// TODO: Make parse (creating params) or accessing params canonicalize param names to all lower-case
	params := parseWwwAuth(resp.Header.Get("Www-Authenticate"))
	// Default to MD5 if algorithm isn't specified in response.
	// Source: Paragraph describing "algorithm" at https://tools.ietf.org/html/rfc7616#section-3.3
	algoString := valueOrDefault(params["algorithm"], "MD5")
	var sess bool
	// Strip "-sess" from algorithm string if present
	if strings.HasSuffix(algoString, "-sess") {
		sess = true
		// This assumes algorithm will never be specified as "-sess", which should hold
		algoString = algoString[:len(algoString)-5]
	}
	var algo func(string) string
	if algo = algorithms[algoString]; algo == nil {
		// Full failure if algorithm can't be resolved; there's no way to continue
		return ""
	}

	realm := params["realm"]
	nonce := params["nonce"]
	cnonce, err := generateClientNonce()
	if err != nil {
		// Refuse to continue if a client nonce can't be generated
		return ""
	}

	// RFC 7616 Section 3.4.2 https://tools.ietf.org/html/rfc7616#section-3.4.2
	var a1 string
	a1Components := []string{unquote(creds.Username), unquote(realm), creds.Password}
	if sess {
		hash := algo(strings.Join(a1Components, ":"))
		a1Components = []string{hash, unquote(nonce), unquote(cnonce)}
		a1 = strings.Join(a1Components, ":")
	} else {
		a1 = strings.Join(a1Components, ":")
	}

	// According to request.go: "For client requests an empty [method] string means GET."
	method := valueOrDefault(req.Method, "GET")
	requestURI := req.URL.RequestURI()
	qopOptions := strings.Split(valueOrDefault(params["qop"], "auth"), ", ")
	// Use first Quality of Protection listed by server
	qop := qopOptions[0]
	// Restores end quote if it was cut off due to truncating a list of values.
	if len(qopOptions) > 1 {
		qop += `"`
	}
	// RFC 7616 Section 3.4.3 https://tools.ietf.org/html/rfc7616#section-3.4.3
	var a2 string
	a2Components := []string{method, requestURI}
	if qop == "auth-int" {
		// TODO: Future: Implement "auth-int" Quality of Protection according to RFC 7616 Section 3.4.3
		return ""
	} else {
		// Execute if qop is "auth" or unspecified
		a2 = strings.Join(a2Components, ":")
	}

	// TODO: Future: Stop hard-coding nc (nonce count) as 1. Somehow keep track of that between requests with a host.
	nc := fmt.Sprintf("%08x", 1)

	dataComponents := []string{unquote(nonce), nc, unquote(cnonce), unquote(qop), algo(a2)}
	response := `"` + keyedDigest(algo, algo(a1), strings.Join(dataComponents, ":")) + `"`

	// Username must be hashes after any other hashing, per RFC 7616 Section 3.4.4
	// TODO: Write logic that determines whether to include username or username*, how to encode that
	// TODO: If the username would necessitate using username*, but hashing is enabled, no * is required. Just send the hash.
	username := creds.Username
	userhash := valueOrDefault(params["userhash"], "false")
	if userhash == "true" {
		username = algo(unquote(username) + ":" + unquote(realm))
	}

	ret := "Digest username=\"" + username +
			"\", realm=" + realm +
			", uri=\"" + requestURI +
			"\", algorithm=" + algoString +
			", nonce=" + nonce +
			", nc=" + nc +
			", cnonce=\"" + cnonce +
			"\", qop=" + qop +
			", response=" + response +
			", userhash=" + userhash
	// Apache refuses request when opaque is empty; output opaque when non-empty
	if opaque := params["opaque"]; opaque != "" {
		ret += ", opaque=" + opaque
	}
	return ret
}

func getBasicAuth(creds *credential) string {
	// Explicitly declare Header so that it's non-nil and can be assigned to
	temp := &http.Request{Header: make(http.Header)}
	temp.SetBasicAuth(creds.Username, creds.Password)
	return temp.Header.Get("Authorization")
}

// TODO: Really nail down what the correct policy is here.
	// 1) There can be a header or not
	// 2) There can be credentials for a host or not
	// 3) Header can contain scheme that's known, unknown, or none
	// 3) A specified scheme can be "Basic" or "Digest"
// TODO: Invert this so that resp is checked before presence of host
func (auther authenticator) TryGetAuth(req *http.Request, resp *http.Response) string {
	// NOTE: If/when wildcards for hosts are introduced, automatically sending
		// Basic Auth to a host that matches the specified format could become
		// problematic, particularly if not implemented very carefully. If
		// "google.com.attacker.net" matches a specified wildcard of "google.com*",
		// a user could unknowingly send Google creds to "attacker.net"
	// TODO: Consider whether taking in https status would be a good precaution,
		// in order to somehow warn about plaintext auth or implement safer defaults
	// TODO: Figure out a good way to get the IP address involved in an http request
	// Otherwise, require the caller pass in the relevant hostname/ip
	// If both are accepted, could list different creds for IP and hostname.
	// Unclear how to resolve that conflict.
	host := req.URL.Hostname()
	creds, ok := auther.creds[host]
	// Credentials were found for the relevant host
	if ok {
		// Response Header exists
		if resp != nil && resp.Header != nil {
			scheme := strings.Split(resp.Header.Get("Www-Authenticate"), " ")[0]
			switch scheme {
			case "Basic":
				return getBasicAuth(creds)
			case "Digest":
				return getDigestAuth(creds, req, resp)
			default:
				return ""
			}
		} else {
			// Guess BasicAuth, avoiding wait for 2nd response if correct
			return getBasicAuth(creds)
		}
	}
	// TODO: Future: Otherwise, assign default creds if those are specified
	return ""
}

// TODO: Handle discrepencies between hostname and ip address
// Currently only allowing hostnames to be used.