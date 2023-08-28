package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

const userAgent = "HIDE.ME.LINUX.CLI-0.9.7"

var ErrAppUpdateRequired = errors.New( "application update required" )
var ErrHttpStatusBad = errors.New( "bad HTTP status" )
var ErrBadPin = errors.New( "bad public key PIN" )
var ErrMissingHost = errors.New( "missing host" )
var ErrBadDomain = errors.New( "bad domain ")

type Config struct {
	APIVersion				string			`yaml:"-"`										// Current API version is 1.0.0
	Host					string			`yaml:"host,omitempty"`							// FQDN of the server
	Port					int				`yaml:"port,omitempty"`							// Port to connect to when issuing REST requests
	Domain					string			`yaml:"domain,omitempty"`						// Domain ( hide.me )
	AccessTokenPath			string			`yaml:"accessTokenPath,omitempty"`				// Access-Token path
	AccessToken				string			`yaml:"accessToken,omitempty"`					// Base64 encoded Access-Token
	Username				string			`yaml:"username,omitempty"`						// Username ( Access-Token takes precedence )
	Password				string			`yaml:"password,omitempty"`						// Password ( Access-Token takes precedence )
	RestTimeout				time.Duration	`yaml:"restTimeout,omitempty"`					// Timeout for REST requests
	ReconnectWait			time.Duration	`yaml:"reconnectWait,omitempty"`				// Reconnect timeout
	AccessTokenUpdateDelay	time.Duration	`yaml:"AccessTokenUpdateDelay,omitempty"`		// Period to wait for when updating a stale Access-Token
	CA						string			`yaml:"CA,omitempty"`							// CA certificate bundle ( empty for system-wide CA roots )
	Mark					int				`yaml:"mark,omitempty"`							// Firewall mark for the traffic generated by this app
	DnsServers				string			`yaml:"dnsServers,omitempty"`					// DNS servers to use when resolving names for client requests ( wireguard link uses it's assigned DNS servers )
	Filter					Filter			`yaml:"filter,omitempty"`						// Filtering settings
}

// SetHost adds .hideservers.net suffix for short names ( nl becomes nl.hideservers.net ) or removes .hide.me and replaces it with .hideservers.net.
func ( c *Config ) SetHost( host string ) {
	c.Host = host
	if net.ParseIP( c.Host ) != nil { return }
	if strings.HasSuffix( c.Host, ".hideservers.net" ) { return }
	c.Host = strings.TrimSuffix( c.Host, ".hide.me" )
	c.Host += ".hideservers.net"
}

type Client struct {
	*Config
	
	client					*http.Client
	resolver				*net.Resolver
	dnsServers				[]string
	remote					*net.TCPAddr
	
	accessToken				[]byte
	authorizedPins			map[string]string
}

func New( config *Config ) *Client { if config == nil { config = &Config{} }; return &Client{ Config: config } }

func ( c *Client ) Init() ( err error ) {
	if c.Config.Port == 0 { c.Config.Port = 432 }
	if c.Port == 443 { c.APIVersion = "v1"; log.Println( "Init: [WARNING] Using port 443, API unstable" ) }
	if c.Domain != "hide.me" { err = ErrBadDomain; return }
	
	dialer := &net.Dialer{}																															// Use a custom dialer to set the socket mark on sockets when configured
	if c.Config.Mark > 0 {
		dialer.Control = func( network, address string, rawConn syscall.RawConn ) ( err error ) {
			if network == "tcp4" && address == "10.255.255.250:4321" { return }																		// Do not set marks for in-tunnel traffic
			_ = rawConn.Control( func( fd uintptr ) {
				err = syscall.SetsockoptInt( int(fd), unix.SOL_SOCKET, unix.SO_MARK, c.Config.Mark )
				if err != nil { log.Println( "Dial: [ERR] Set mark failed:", err ) }
			})
			return
		}
	}
	c.resolver = &net.Resolver{ PreferGo: true, Dial: func(ctx context.Context, network string, addr string) ( net.Conn, error ) {
		return dialer.DialContext( ctx, network, c.dnsServers[ rand.Intn( len( c.dnsServers ) ) ] )
	}}
	dialer.Resolver = c.resolver
	
	transport := &http.Transport{																													// HTTPS client setup
		DialContext: 			dialer.DialContext,
		TLSHandshakeTimeout:	time.Second * 5,
		DisableKeepAlives:		true,
		ResponseHeaderTimeout:	time.Second * 5,
		ForceAttemptHTTP2:		true,
		TLSClientConfig: &tls.Config{
			NextProtos:				[]string{ "h2" },
			ServerName:				"hideservers.net",																								// hideservers.net is always a certificate SAN
			MinVersion:				tls.VersionTLS13,
			VerifyPeerCertificate:	c.Pins,
		},
	}
	if len( c.Config.CA ) > 0 {
		pem, err := os.ReadFile( c.Config.CA ); if err != nil { return err }
		transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		if ok := transport.TLSClientConfig.RootCAs.AppendCertsFromPEM( pem ); !ok { return errors.New( "bad certificate in " + c.Config.CA ) }
	}
	c.client = &http.Client{
		Transport:	transport,
		Timeout:	c.Config.RestTimeout,
	}
	
	if len( c.Config.DnsServers ) > 0 {																												// DNS setup
		for _, dnsServer := range strings.Split( c.Config.DnsServers, "," ) {
			c.dnsServers = append( c.dnsServers, strings.TrimSpace( dnsServer ) )
		}
	} else { c.dnsServers = append( c.dnsServers, "1.1.1.1:53" ) }
	
	if len( c.Config.AccessToken ) > 0 {																											// Access-Token
		if c.accessToken, err = base64.StdEncoding.DecodeString( c.Config.AccessToken ); err != nil { return }
	}
	if c.accessToken == nil && len( c.Config.AccessTokenPath ) > 0 {
		if accessTokenBytes, err := os.ReadFile( c.Config.AccessTokenPath ); err == nil {
			if c.accessToken, err = base64.StdEncoding.DecodeString( string( accessTokenBytes ) ); err != nil { return err }
		}
	}
	c.Config.Filter.AccessToken = c.accessToken
	
	c.authorizedPins = map[string]string{																											// Certificate names and pins
		"Hide.Me Root CA": "AdKh8rXi68jeqv5kEzF4wJ9M2R89gFuMILRQ1uwADQI=",
		"Hide.Me Server CA #1": "CsEyDelMHMPh9qLGgeQn8sJwdUwvc+fCMhOU9Ne5PbU=",
		"DigiCert Global Root CA": "r/mIkG3eEpVdm+u/ko/cwxzOMo1bk4TyHIlByibiA5E=",
		"DigiCert TLS RSA SHA256 2020 CA1": "RQeZkB42znUfsDIIFWIRiYEcKl7nHwNFwWCrnMMJbVc=",
	}
	return
}

func ( c *Client ) Remote() *net.TCPAddr { return c.remote }

// Pins checks public key pins of authorized hide.me/hideservers.net CA certificates
func ( c *Client ) Pins( _ [][]byte, verifiedChains [][]*x509.Certificate) error {
	for _, chain := range verifiedChains {
		chainLoop:
		for _, certificate := range chain {
			if !certificate.IsCA { continue }
			sum := sha256.Sum256( certificate.RawSubjectPublicKeyInfo )
			pin := base64.StdEncoding.EncodeToString( sum[:] )
			for name, authorizedPin := range c.authorizedPins {
				if certificate.Subject.CommonName == name && pin == authorizedPin {
					log.Println( "Pins:", certificate.Subject.CommonName, "pin OK" )
					continue chainLoop
				}
			}
			log.Println( "Pins:", certificate.Subject.CommonName, "pin failed" )
			return ErrBadPin
		}
	}
	return nil
}

func ( c *Client ) postJson( ctx context.Context, url string, object interface{} ) ( responseBody []byte, err error ) {
	body, err := json.MarshalIndent( object, "", "\t" )
	if err != nil { return }
	request, err := http.NewRequestWithContext( ctx, "POST", url, bytes.NewReader( body ) )
	if err != nil { return }
	request.Header.Set( "user-agent", userAgent )
	request.Header.Add( "content-type", "application/json" )
	response, err := c.client.Do( request )
	if err != nil { return }
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden { log.Println( "Rest: [ERR] Application update required" ); return nil, ErrAppUpdateRequired }
	if response.StatusCode != http.StatusOK { log.Println( "Rest: [ERR] Bad HTTP response (", response.StatusCode, ")" ); err = ErrHttpStatusBad; return }
	return io.ReadAll( response.Body )
}

func ( c *Client ) get( ctx context.Context, url string ) ( responseBody []byte, err error ) {
	request, err := http.NewRequestWithContext( ctx, "GET", url, nil )
	if err != nil { return }
	request.Header.Set( "user-agent", userAgent )
	response, err := c.client.Do( request )
	if err != nil { return }
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK { log.Println( "Rest: [ERR] Bad HTTP response (", response.StatusCode, ")" ); err = ErrHttpStatusBad; return }
	return io.ReadAll( response.Body )
}

func ( c *Client ) HaveAccessToken() bool { if c.accessToken != nil { return true }; return false }

// Resolve resolves an IP of a Hide.me endpoint and stores that IP for further use. Hide.me balances DNS rapidly, so once an IP is acquired it needs to be used for the remainder of the session
func ( c *Client ) Resolve( ctx context.Context ) ( err error ) {
	if len( c.Host ) == 0 { err = ErrMissingHost; return }
	if ip := net.ParseIP( c.Config.Host ); ip != nil { c.remote = &net.TCPAddr{ IP: ip, Port: c.Config.Port }; return }								// c.Host is an IP address, set remote endpoint to that IP
	addrs, err := c.resolver.LookupIPAddr( ctx, c.Config.Host )
	if err != nil {																																	// If DNS fails during reconnect then the remote server address in c.remote will be reused for the reconnection attempt
		log.Println( "Name: [ERR]", c.Config.Host, "lookup failed:", err )
		if c.remote != nil { log.Println( "Name: Using previous lookup response", c.remote.String() ); return nil }
		return
	}
	if len( addrs ) == 0 { return errors.New( "dns lookup failed for " + c.Config.Host ) }
	if addrs[0].IP == nil { return errors.New( "no IP found for " + c.Config.Host ) }
	c.remote = &net.TCPAddr{ IP: addrs[0].IP, Port: c.Config.Port }
	log.Println( "Name: Resolved", c.Config.Host, "to", c.remote.IP )
	return
}

// Connect issues a connect request to a Hide.me "Connect" endpoint which expects an ordinary POST request with a ConnectRequest JSON payload
func ( c *Client ) Connect( ctx context.Context, key wgtypes.Key ) ( connectResponse *ConnectResponse, err error ) {
	if len( c.Host ) == 0 { err = ErrMissingHost; return }
	connectRequest := &ConnectRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		AccessToken:	c.accessToken,
		PublicKey:		key[:],
	}
	if err = connectRequest.Check(); err != nil { return }
	
	responseBody, err := c.postJson( ctx, "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/connect", connectRequest )
	if err != nil { return }
	
	connectResponse = &ConnectResponse{}
	err = json.Unmarshal( responseBody, connectResponse )
	return
}

// Disconnect issues a disconnect request to a Hide.me "Disconnect" endpoint which expects an ordinary POST request with a DisconnectRequest JSON payload
func ( c *Client ) Disconnect( ctx context.Context, sessionToken []byte ) ( err error ) {
	if len( c.Host ) == 0 { err = ErrMissingHost; return }
	disconnectRequest := &DisconnectRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		SessionToken:	sessionToken,
	}
	if err = disconnectRequest.Check(); err != nil { return }
	
	_, err = c.postJson( ctx, "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/disconnect", disconnectRequest )
	return
}

// GetAccessToken issues an AccessToken request to a Hide.me "AccessToken" endpoint which expects an ordinary POST request with a AccessTokenRequest JSON payload
func ( c *Client ) GetAccessToken( ctx context.Context ) ( accessToken string, err error ) {
	if len( c.Host ) == 0 { err = ErrMissingHost; return }
	accessTokenRequest := &AccessTokenRequest{
		Host:			strings.TrimSuffix( c.Config.Host, ".hideservers.net" ),
		Domain:			c.Config.Domain,
		AccessToken:	c.accessToken,
		Username:		c.Config.Username,
		Password:		c.Config.Password,
	}
	if err = accessTokenRequest.Check(); err != nil { return }
	
	accessTokenJson, err := c.postJson( ctx, "https://" + c.remote.String() + "/" + c.Config.APIVersion + "/accessToken", accessTokenRequest )
	if err != nil { return }
	
	if err = json.Unmarshal( accessTokenJson, &accessToken ); err != nil { return }
	if c.accessToken, err = base64.StdEncoding.DecodeString( accessToken ); err != nil { return }
	
	if len( c.Config.AccessTokenPath ) > 0 { err = os.WriteFile( c.Config.AccessTokenPath, []byte( accessToken ), 0600 ) }
	return
}

func ( c *Client ) ApplyFilter( ctx context.Context ) ( err error ) {
	if err = c.Config.Filter.Check(); err != nil { return }
	response, err := c.postJson( ctx, "https://10.255.255.250:4321/filter", c.Config.Filter )
	if string(response) == "false" { err = errors.New( "filter failed" ) }
	return
}

func ( c *Client ) FetchCategoryList( ctx context.Context ) ( err error ) {
	response, err := c.get( ctx, "https://" + c.remote.String() + "/categorization/categories.json" )
	if err != nil { return }
	
	type Category struct {
		Name		string
		Description	string
	}
	cats := []Category{}
	if err = json.Unmarshal( response, &cats ); err != nil { return }
	
	log.Printf( "%40s | %s\n", "CATEGORY", "DESCRIPTION" )
	log.Printf( "%40s | %s\n", "--------", "-----------" )
	for _, cat := range cats { log.Printf( "%40s | %s\n", cat.Name, cat.Description ) }
	return
}