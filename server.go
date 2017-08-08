package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
)

var (
	// TODO: Make configurable
	tcpTimeout = 10 * time.Second
	// TODO: Make configurable + match wire protocol
	maxBufSize = 512
)

type PostgresServer struct {
	listener net.Listener

	waitGroup *sync.WaitGroup

	hpfeedsChan    chan []byte
	hpfeedsEnabled bool

	addr string
	port string

	pgUsers   map[string]bool
	cleartext bool
}

func NewPostgresServer(port string, addr string, users []string, cleartext bool, hpfeedsChan chan []byte, hpfeedsEnabled bool) *PostgresServer {
	listener, err := net.Listen("tcp", addr+":"+port)
	if err != nil {
		log.Errorf("Error listening: %s", err)
		os.Exit(1)
	}

	pgUsers := map[string]bool{}
	for _, u := range users {
		pgUsers[u] = true
	}

	return &PostgresServer{
		listener:       listener,
		waitGroup:      new(sync.WaitGroup),
		hpfeedsChan:    hpfeedsChan,
		hpfeedsEnabled: hpfeedsEnabled,
		addr:           addr,
		port:           port,
		cleartext:      cleartext,
		pgUsers:        pgUsers,
	}
}

func (p *PostgresServer) Close() {
	p.waitGroup.Wait()
	p.listener.Close()
}

func (p *PostgresServer) Listen() {
	log.Infof("Starting to listening on %s:%s...", p.addr, p.port)
	for {
		//FIXME: conn is an example of primitive obsession
		conn, err := p.listener.Accept()
		if err != nil {
			log.Warn("Error accepting: %s", err)
			continue
		}

		conn.SetDeadline(time.Now().Add(tcpTimeout))

		p.waitGroup.Add(1)
		go p.handleRequest(conn)
	}
}

func handleError(err error) {
	if err != io.EOF {
		operr, ok := err.(*net.OpError)
		if ok && operr.Timeout() {
			log.Info("Timed out when reading buffer. Err: %s", err)
			return
		}

		log.Warn("Error reading buffer. Err: %s", err)
	}
}

// PostgresServer receives TCP Connections and creates instances of PostgresConnections
// PostgresConnections then figure out how to respond to the request by checking its current state.
// It then asks the PostgresResponder to respond

func (p *PostgresServer) handleRequest(conn net.Conn) {
	defer p.waitGroup.Done()
	defer conn.Close()

	//FIXME: What is this?
	sentStartup := false

	buf := make([]byte, maxBufSize)
	for {
		//FIXME: buf is an example of primitive obsession
		_, err := conn.Read(buf)
		if err != nil {
			handleError(err)
			break
		}

		//FIXME: Move to it's own func
		// Send to hpfeeds if turned on
		if p.hpfeedsEnabled {
			sourceAddr := conn.RemoteAddr().String()
			event := HpFeedsEvent{
				Packet:     buf,
				SourceIP:   strings.Split(sourceAddr, ":")[0],
				SourcePort: strings.Split(sourceAddr, ":")[1],
				DestIP:     p.addr,
				DestPort:   p.port,
			}

			eventJson, err := json.Marshal(event)
			if err != nil {
				log.Errorf("Error sending event to hpfeeds. Err: %s", err)
				continue
			}

			select {
			case p.hpfeedsChan <- eventJson:
				log.Debug("Sent event to hpfeeds")
			default:
				log.Warn("Channel full, discarding message - check HPFeeds configuration")
				log.Infof("Discarded buffer: %s", buf)
			}
		}

		//FIXME: Remove conditional complexity
		if isSSLRequest(buf) {
			log.Debug("Got ssl request...")
			conn.Write([]byte("N"))
			continue
		}

		if !sentStartup {
			log.Debug("Handling startup message...")
			ok := p.handleStartup(buf, conn)
			if !ok {
				break
			}
			sentStartup = true
			continue
		}

		buffer := readBuf(buf)
		pktType := buffer.string()

		if pktType == "p" {
			log.Debug("Handling password...")
			handlePassword(buffer, conn)
			break
		} else {
			// TODO
			log.Info("TODO")
		}
	}
}

// Initial requests:
// 	SSL Request - 00 00 00 08 04 d2 16 2f
func isSSLRequest(payload []byte) bool {
	//FIXME: Label magic number
	if bytes.Compare(payload[:8], []byte{0, 0, 0, 8, 4, 210, 22, 47}) == 0 {
		return true
	}
	return false
}

// -1 means everything is null
func indexOfLastFilledByte(buf readBuf) int {
	for i := 0; i < len(buf); i += 4 {
		word := buf[i : i+4]
		if isNullWord(word) {
			return i - numberOfTrailingNulls(buf[i-4:i])
		}
	}
	return len(buf) - 1
}

// Takes a word like: %v[108, 0, 0, 0] and returns 3, the number of trailing nulls.
func numberOfTrailingNulls(word []byte) int {
	counter := 0
	for i := len(word) - 1; i >= 0; i-- {
		if word[i] == 0 {
			counter++
		} else {
			return counter
		}
	}
	return counter
}

func isNullWord(word []byte) bool {
	for _, v := range word {
		if v != 0 {
			return false
		}
	}
	return true
}

func (p *PostgresServer) handleStartup(buff readBuf, conn net.Conn) bool {
	buf := readBuf(buff)
	// Actual length finds the last byte and then adds two, because there is two null terminators at the end of the packet.
	actualLength := indexOfLastFilledByte(buf) + 2
	claimedLength := buf.int32()

	if (actualLength == 0) || (claimedLength != actualLength) {
		log.Debugf("Invalid handshake request received from %s, ", conn.RemoteAddr())
		log.Debugf("claimed length: %d, actual length: %d", claimedLength, actualLength)
		log.Debugf("Packet contents: %v", buff)
		conn.Write(handshakeErrorResponse())
		return true
	}
	_ = buf.int32()

	startupMap := map[string]string{}
	for len(buf) > 1 {
		k := buf.string()
		v := buf.string()
		startupMap[k] = v
	}

	if p.pgUsers[startupMap["user"]] {
		// TODO: Support multiple auth types
		// Looking for requesting cleartext passwords would be a good way to finger print
		// pghoney. We should have md5 be the default since it is the postgres default.
		if p.cleartext {
			//FIXME: Bad names
			conn.Write(cleartextAuthResponse())
		} else {
			//FIXME: Bad names
			conn.Write(md5AuthResponse())
		}
		return true
	}

	conn.Write(userDoesntExistResponse(startupMap["user"]))
	return false
}

func cleartextAuthResponse() []byte {
	buf := authResponsePrefix()
	// cleartext
	buf.int32(3)
	return buf.wrap()
}

func md5AuthResponse() []byte {
	buf := authResponsePrefix()
	// md5
	buf.int32(5)
	// Byte4 - "The salt to use when encrypting the password."
	// FIXME: Don't hardcode the salt
	buf.bytes([]byte{51, 111, 191, 210})
	return buf.wrap()
}

func authResponsePrefix() *writeBuf {
	return &writeBuf{
		buf: []byte{'R', 0, 0, 0, 0},
		pos: 1,
	}
}

func handlePassword(buf readBuf, conn net.Conn) {
	// TODO: Save somewhere
	conn.Write(authFailedResponse())
}

func authFailedResponse() []byte {
	return authErrorResponse("Auth failed")
}

func userDoesntExistResponse(user string) []byte {
	return authErrorResponse("No such user: " + user)
}

// Taken from network capture and https://www.postgresql.org/docs/9.3/static/protocol-error-fields.html
func authErrorResponse(message string) []byte {
	buf := errorResponsePrefix()
	// Severity
	buf.string("SERROR")
	// Code & Position
	buf.string("C08P01")
	// Message
	buf.string("M" + message + "\000")
	return buf.wrap()
}

// Taken from a tcpdump of an nmap scan error
func handshakeErrorResponse() []byte {
	buf := errorResponsePrefix()
	// Severity
	buf.string("SERROR")
	// Code
	buf.string("C0A000")
	// Message - TODO: make more dynamic
	buf.string("Munsupported frontend protocol 65363.19778: server supports 1.0 to 3.0")
	// File
	buf.string("Fpostmaster.c")
	// Line
	buf.string("L2005")
	// Routine
	buf.string("RProcessStartupPacket" + "\000")
	return buf.wrap()
}

func errorResponsePrefix() *writeBuf {
	return &writeBuf{
		buf: []byte{'E', 0, 0, 0, 0},
		pos: 1,
	}
}
