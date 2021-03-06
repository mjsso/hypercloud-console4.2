package proxy

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var websocketPingIntervalCloud = 30 * time.Second
var websocketTimeoutCloud = 30 * time.Second

type ConfigCloud struct {
	HeaderBlacklist []string
	Endpoint        *url.URL
	TLSClientConfig *tls.Config
	Origin          string
}

type ProxyCloud struct {
	reverseProxy *httputil.ReverseProxy
	config       *Config
}

func filterHeadersCloud(r *http.Response) error {
	badHeaders := []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade"}
	for _, h := range badHeaders {
		r.Header.Del(h)
	}
	return nil
}

func NewProxyCloud(cfg *Config) *Proxy {
	// Copy of http.DefaultTransport with TLSClientConfig added
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSClientConfig:     cfg.TLSClientConfig,
		TLSHandshakeTimeout: 30 * time.Second,
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(cfg.Endpoint)
	reverseProxy.FlushInterval = time.Millisecond * 100
	reverseProxy.Transport = transport
	reverseProxy.ModifyResponse = filterHeadersCloud

	proxy := &Proxy{
		reverseProxy: reverseProxy,
		config:       cfg,
	}

	return proxy
}

func SingleJoiningSlashCloud(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// decodeSubprotocol decodes the impersonation "headers" on a websocket.
// Subprotocols don't allow '=' or '/'
func decodeSubprotocolCloud(encodedProtocol string) (string, error) {
	encodedProtocol = strings.Replace(encodedProtocol, "_", "=", -1)
	encodedProtocol = strings.Replace(encodedProtocol, "-", "/", -1)
	decodedProtocol, err := base64.StdEncoding.DecodeString(encodedProtocol)
	return string(decodedProtocol), err
}

// var headerBlacklistCloud = []string{"Cookie", "X-CSRFToken"}

var headerBlacklistCloud = []string{"X-CSRFToken"}

// hypercloud websocket connection 전용 proxy 코드
func (p *Proxy) ServeHTTPCloud(w http.ResponseWriter, r *http.Request) {
	// sandbox 사용 주석처리
	// // Block scripts from running in proxied content for browsers that support Content-Security-Policy.
	// w.Header().Set("Content-Security-Policy", "sandbox;")
	// // Add `X-Content-Security-Policy` for IE11 and older browsers.
	// w.Header().Set("X-Content-Security-Policy", "sandbox;")

	isWebsocket := false
	upgrades := r.Header["Upgrade"]

	for _, upgrade := range upgrades {
		if strings.ToLower(upgrade) == "websocket" {
			isWebsocket = true
			break
		}
	}

	for _, h := range headerBlacklistCloud {
		r.Header.Del(h)
	}

	if !isWebsocket {
		p.reverseProxy.ServeHTTP(w, r)
		return
	}

	r.Host = p.config.Endpoint.Host
	r.URL.Host = p.config.Endpoint.Host
	r.URL.Path = SingleJoiningSlash(p.config.Endpoint.Path, r.URL.Path)
	r.URL.Scheme = p.config.Endpoint.Scheme

	if r.URL.Scheme == "https" {
		r.URL.Scheme = "wss"
	} else {
		r.URL.Scheme = "ws"
	}

	subProtocol := ""
	proxiedHeader := make(http.Header, len(r.Header))
	for key, value := range r.Header {
		if key != "Sec-Websocket-Protocol" {
			// Do not proxy the subprotocol to the API server because k8s does not understand what we're sending
			proxiedHeader.Set(key, r.Header.Get(key))
			continue
		}

		for _, protocols := range value {
			for _, protocol := range strings.Split(protocols, ",") {
				protocol = strings.TrimSpace(protocol)
				// proxy header에 필요 없는 해더 내용 주석처리
				// TODO: secure by stripping newlines & other invalid stuff
				// "Impersonate-User" and "Impersonate-Group" and bridge specific (not a k8s thing)
				// if strings.HasPrefix(protocol, "Impersonate-User.") {
				// 	encodedProtocol := strings.TrimPrefix(protocol, "Impersonate-User.")
				// 	decodedProtocol, err := decodeSubprotocolCloud(encodedProtocol)
				// 	if err != nil {
				// 		errMsg := fmt.Sprintf("Error decoding Impersonate-User subprotocol: %v", err)
				// 		http.Error(w, errMsg, http.StatusBadRequest)
				// 		return
				// 	}
				// 	proxiedHeader.Set("Impersonate-User", decodedProtocol)
				// 	subProtocol = protocol
				// } else if strings.HasPrefix(protocol, "Impersonate-Group.") {
				// 	encodedProtocol := strings.TrimPrefix(protocol, "Impersonate-Group.")
				// 	decodedProtocol, err := decodeSubprotocolCloud(encodedProtocol)
				// 	if err != nil {
				// 		errMsg := fmt.Sprintf("Error decoding Impersonate-Group subprotocol: %v", err)
				// 		http.Error(w, errMsg, http.StatusBadRequest)
				// 		return
				// 	}
				// 	proxiedHeader.Set("Impersonate-User", string(decodedProtocol))
				// 	proxiedHeader.Set("Impersonate-Group", string(decodedProtocol))
				// 	subProtocol = protocol
				// } else {
				proxiedHeader.Set("Sec-Websocket-Protocol", protocol)
				subProtocol = protocol
				// }
			}
		}
	}

	// Filter websocket headers.
	websocketHeaders := []string{
		"Connection",
		"Sec-Websocket-Extensions",
		"Sec-Websocket-Key",
		// NOTE: kans - Sec-Websocket-Protocol must be proxied in the headers
		"Sec-Websocket-Version",
		"Upgrade",
	}
	for _, header := range websocketHeaders {
		proxiedHeader.Del(header)
	}

	// NOTE (ericchiang): K8s might not enforce this but websockets requests are
	// required to supply an origin.
	proxiedHeader.Add("Origin", "http://localhost")

	// NOTE: bearer token 넣어보기 위해 Authorization 추가 // 정동민
	token, ok := r.URL.Query()["token"]
	if ok && len(token[0]) > 0 {
		proxiedHeader.Add("Authorization", "Bearer "+string(token[0]))
	}
	// NOTE: 여기까지
	dialer := &websocket.Dialer{
		TLSClientConfig: p.config.TLSClientConfig,
	}

	backend, resp, err := dialer.Dial(r.URL.String(), proxiedHeader)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to dial backend: '%v'", err)
		statusCode := http.StatusBadGateway
		if resp == nil || resp.StatusCode == 0 {
			log.Println(errMsg)
		} else {
			statusCode = resp.StatusCode
			if resp.Request == nil {
				log.Printf("%s Status: '%v' (no request object)", errMsg, resp.Status)
			} else {
				log.Printf("%s Status: '%v' URL: '%v'", errMsg, resp.Status, resp.Request.URL)
			}
		}
		http.Error(w, errMsg, statusCode)
		return
	}
	defer backend.Close()

	upgrader := &websocket.Upgrader{
		Subprotocols: []string{subProtocol},
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header["Origin"]
			if p.config.Origin == "" {
				log.Printf("CheckOrigin: Proxy has no configured Origin. Allowing origin %v to %v", origin, r.URL)
				return true
			}
			if len(origin) == 0 {
				log.Printf("CheckOrigin: No origin header. Denying request to %v", r.URL)
				return false
			}
			// if p.config.Origin == origin[0] {
			// 	return true
			// }

			// NOTE: 아래 코드는 다음 에러를 회피하기 위해 삽입되었다. // 정동민
			// CheckOrigin 'https://192.168.8.27' != 'https://192.168.8.27:31303'
			// Failed to upgrade websocket to client: 'websocket: request origin not allowed by Upgrader.CheckOrigin'

			pOriginParsed, _ := url.Parse(p.config.Origin)
			pHost, _, _ := net.SplitHostPort(pOriginParsed.Host)

			rOriginParsed, _ := url.Parse(origin[0])
			rHost, _, _ := net.SplitHostPort(rOriginParsed.Host)

			if pHost == rHost {
				return true
			}
			// NOTE: 여기까지
			log.Printf("CheckOrigin '%v' != '%v'", p.config.Origin, origin[0])
			return false
		},
	}
	frontend, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade websocket to client: '%v'", err)
		return
	}

	ticker := time.NewTicker(websocketPingInterval)
	var writeMutex sync.Mutex // Needed because ticker & copy are writing to frontend in separate goroutines

	defer func() {
		ticker.Stop()
		frontend.Close()
	}()

	errc := make(chan error, 2)

	// Can't just use io.Copy here since browsers care about frame headers.
	go func() { errc <- copyMsgsCloud(nil, frontend, backend) }()
	go func() { errc <- copyMsgsCloud(&writeMutex, backend, frontend) }()

	for {
		select {
		case <-errc:
			// Only wait for a single error and let the defers close both connections.
			return
		case <-ticker.C:
			writeMutex.Lock()
			// Send pings to client to prevent load balancers and other middlemen from closing the connection early
			err := frontend.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(websocketTimeout))
			writeMutex.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func copyMsgsCloud(writeMutex *sync.Mutex, dest, src *websocket.Conn) error {
	for {
		messageType, msg, err := src.ReadMessage()
		if err != nil {
			return err
		}

		if writeMutex == nil {
			err = dest.WriteMessage(messageType, msg)
		} else {
			writeMutex.Lock()
			err = dest.WriteMessage(messageType, msg)
			writeMutex.Unlock()
		}

		if err != nil {
			return err
		}
	}
}
