package main

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/miekg/dns"
	"golang.org/x/time/rate"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var config *Config

// Config represents the structure of the configuration file.
type Config struct {
	Host    string            `json:"host"`
	Domains map[string]string `json:"domains"`
}

// LoadConfig loads the configuration from a JSON file.
func LoadConfig(filename string) (*Config, error) {
	var config Config
	cfgBytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(cfgBytes, &config)
	return &config, err
}

func findValueByKeyContains(m map[string]string, substr string) (string, bool) {
	for key, value := range m {
		if strings.Contains(strings.ToLower(substr), strings.ToLower(key)) {
			return value, true
		}
	}
	return "", false // Return empty string and false if no key contains the substring
}

// processDNSQuery processes the DNS query and returns a response.
func processDNSQuery(query []byte) ([]byte, error) {
	var msg dns.Msg
	err := msg.Unpack(query)
	if err != nil {
		return nil, err
	}

	if len(msg.Question) == 0 {
		return nil, fmt.Errorf("no DNS question found in the request")
	}

	domain := msg.Question[0].Name
	if ip, ok := findValueByKeyContains(config.Domains, domain); ok {
		rr, err := dns.NewRR(domain + " A " + ip)
		if err != nil {
			return nil, err
		}
		msg.Answer = append(msg.Answer, rr)
	} else {
		resp, err := http.Post("https://1.1.1.1/dns-query", "application/dns-message", bytes.NewReader(query))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	return msg.Pack()
}

// handleDoHRequest processes the DoH request with rate limiting.
func handleDoHRequest(limiter *rate.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusInternalServerError)
			return
		}

		dnsResponse, err := processDNSQuery(body)
		if err != nil {
			http.Error(w, "Failed to pack DNS response", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(dnsResponse)
	}
}

// handleDoTConnection handles a single DoT connection.
func handleDoTConnection(conn net.Conn, limiter *rate.Limiter) {
	defer conn.Close()

	if !limiter.Allow() {
		// Log rate limit exceeded
		return
	}

	// Read the first two bytes to determine the length of the DNS message
	lengthBuf := make([]byte, 2)
	_, err := io.ReadFull(conn, lengthBuf)
	if err != nil {
		log.Println(err)
		return
	}

	// Parse the length of the DNS message
	dnsMessageLength := binary.BigEndian.Uint16(lengthBuf)

	// Allocate a buffer of the size indicated by the length and read the DNS message
	buffer := make([]byte, dnsMessageLength)
	_, err = io.ReadFull(conn, buffer)
	if err != nil {
		log.Println(err)
		return
	}

	// Process the DNS query and generate a response
	response, err := processDNSQuery(buffer) // Process the full message
	if err != nil {
		log.Println(err)
		return
	}

	// Prepare the response with the length header
	responseLength := make([]byte, 2)
	binary.BigEndian.PutUint16(responseLength, uint16(len(response)))

	// Write the length of the response followed by the response itself
	_, err = conn.Write(responseLength)
	if err != nil {
		log.Println(err)
		return
	}

	_, err = conn.Write(response)
	if err != nil {
		log.Println(err)
		return
	}
}

// startDoTServer starts the DNS-over-TLS server.
func startDoTServer(limiter *rate.Limiter) {
	// Load TLS credentials
	certPrefix := "/etc/letsencrypt/live/" + config.Host + "/"
	cer, err := tls.LoadX509KeyPair(certPrefix+"/fullchain.pem", certPrefix+"privkey.pem")
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cer}}

	listener, err := tls.Listen("tcp", ":853", tlsConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go handleDoTConnection(conn, limiter)
	}
}

func serveSniProxy() {
	l, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handleConnection(conn)
	}
}

func peekClientHello(reader io.Reader) (*tls.ClientHelloInfo, io.Reader, error) {
	peekedBytes := new(bytes.Buffer)
	hello, err := readClientHello(io.TeeReader(reader, peekedBytes))
	if err != nil {
		return nil, nil, err
	}
	return hello, peekedBytes, nil
}

type readOnlyConn struct {
	reader io.Reader
}

func (conn readOnlyConn) Read(p []byte) (int, error)  { return conn.reader.Read(p) }
func (conn readOnlyConn) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }
func (conn readOnlyConn) Close() error {
	if tcpConn, ok := conn.reader.(io.Closer); ok {
		return tcpConn.Close()
	}
	return nil
}
func (conn readOnlyConn) LocalAddr() net.Addr                { return nil }
func (conn readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (conn readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (conn readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (conn readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

func readClientHello(reader io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo
	var wg sync.WaitGroup

	// Set the wait group for one operation (Handshake)
	wg.Add(1)

	config := &tls.Config{
		GetConfigForClient: func(argHello *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = argHello // Capture the ClientHelloInfo
			wg.Done()        // Indicate that the handshake is complete
			return nil, nil
		},
	}

	tlsConn := tls.Server(readOnlyConn{reader: reader}, config)
	err := tlsConn.Handshake()

	// Wait for the handshake to be captured
	wg.Wait()

	if hello == nil {
		return nil, err
	}

	return hello, nil
}

func handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	if err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Print(err)
		return
	}

	clientHello, clientHelloBytes, err := peekClientHello(clientConn)
	if err != nil {
		log.Print(err)
		return
	}

	if err := clientConn.SetReadDeadline(time.Time{}); err != nil {
		log.Print(err)
		return
	}

	targetHost := strings.ToLower(clientHello.ServerName)

	if targetHost == config.Host {
		targetHost = net.JoinHostPort(targetHost, "8443")
	} else {
		targetHost = net.JoinHostPort(targetHost, "443")
	}

	backendConn, err := net.DialTimeout("tcp", targetHost, 5*time.Second)
	if err != nil {
		log.Print(err)
		return
	}
	defer backendConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		io.Copy(clientConn, backendConn)
		clientConn.(*net.TCPConn).CloseWrite()
		wg.Done()
	}()
	go func() {
		io.Copy(backendConn, clientHelloBytes)
		io.Copy(backendConn, clientConn)
		backendConn.(*net.TCPConn).CloseWrite()
		wg.Done()
	}()

	wg.Wait()
}

func runDOHServer(limiter *rate.Limiter) {
	http.HandleFunc("/dns-query", handleDoHRequest(limiter))

	server := &http.Server{
		Addr:         "127.0.0.1:8080",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Println(server.ListenAndServe())
}

func main() {
	err := os.Setenv("GOGC", "50")
	if err != nil {
		log.Fatal(err)
	} // Set GOGC to 50 to make GC more aggressive

	cfg, err := LoadConfig("config.json")
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	config = cfg

	log.Println("Starting SSNI proxy server on :443, :853...")

	var wg sync.WaitGroup
	wg.Add(3)

	limiter := rate.NewLimiter(10, 50) // 1 request per second with a burst size of 5

	go func() {
		runDOHServer(limiter)
		wg.Done()
	}()
	go func() {
		startDoTServer(limiter)
		wg.Done()
	}()
	go func() {
		serveSniProxy()
		wg.Done()
	}()

	wg.Wait()
}
