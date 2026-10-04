package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type POP3Server struct {
	addr      string
	store     *Store
	config    Config
	listener  net.Listener
	semaphore chan struct{}
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

func NewPOP3Server(config Config, store *Store) *POP3Server {
	return &POP3Server{
		addr:      config.POP3Addr,
		store:     store,
		config:    config,
		semaphore: make(chan struct{}, config.MaxConns),
		conns:     make(map[net.Conn]struct{}),
	}
}

func (s *POP3Server) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = listener
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			select {
			case s.semaphore <- struct{}{}:
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer func() { <-s.semaphore }()
					s.addConn(conn)
					s.handle(conn)
					s.removeConn(conn)
				}()
			default:
				fmt.Fprintf(conn, "-ERR connection limit exceeded\r\n")
				conn.Close()
			}
		}
	}()
	return nil
}

func (s *POP3Server) Shutdown(ctx context.Context) error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *POP3Server) addConn(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *POP3Server) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

type pop3Session struct {
	user      string
	authed    bool
	recipient string
	messages  []MaildropMessage
	deleted   []bool
}

func (s *POP3Server) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	session := &pop3Session{}
	defer func() {
		if session.recipient != "" {
			s.store.UnlockMaildrop(session.recipient)
		}
	}()
	if writePOP3(conn, s.config.ReadTimeout, "+OK POP3 service ready") {
		return
	}

	for {
		line, err := readStrictLine(conn, reader, s.config.ReadTimeout, s.config.MaxLineBytes)
		if err != nil {
			return
		}
		verb, arguments := parsePOP3Command(line)
		var reply string
		closeAfter := false

		if session.authed {
			switch verb {
			case "STAT":
				if arguments != "" {
					reply = "-ERR STAT takes no arguments"
				} else {
					reply = session.stat()
				}
			case "LIST":
				reply = session.list(arguments)
			case "UIDL":
				reply = session.uidl(arguments)
			case "RETR":
				if index, ok := session.messageFor(arguments); ok {
					if s.writeMessage(conn, session.recipient, session.messages[index].ID) {
						return
					}
					continue
				}
				reply = "-ERR no such message"
			case "DELE":
				if index, ok := session.messageFor(arguments); ok {
					session.deleted[index] = true
					reply = "+OK message marked deleted"
				} else {
					reply = "-ERR no such message"
				}
			case "RSET":
				if arguments != "" {
					reply = "-ERR RSET takes no arguments"
				} else {
					for i := range session.deleted {
						session.deleted[i] = false
					}
					reply = "+OK"
				}
			case "NOOP":
				if arguments != "" {
					reply = "-ERR NOOP takes no arguments"
				} else {
					reply = "+OK"
				}
			case "QUIT":
				if arguments != "" {
					reply = "-ERR QUIT takes no arguments"
					break
				}
				if reply = s.commitSession(session); reply == "" {
					reply = "+OK"
				}
				closeAfter = true
			default:
				reply = "-ERR command not recognized"
			}
		} else {
			switch verb {
			case "USER":
				if session.user != "" || strings.ContainsAny(arguments, " \t") {
					reply = "-ERR invalid USER"
				} else if canonical, err := CanonicalRecipient(arguments); err != nil {
					reply = "-ERR invalid mailbox"
				} else if _, allowed := s.config.Recipients[canonical]; !allowed {
					reply = "-ERR invalid mailbox"
				} else {
					session.user = canonical
					reply = "+OK mailbox accepted, send PASS"
				}
			case "PASS":
				if session.user == "" {
					reply = "-ERR send USER first"
				} else if arguments != s.config.POP3Password {
					reply = "-ERR invalid credentials"
				} else if !s.store.TryLockMaildrop(session.user) {
					reply = "-ERR mailbox in use"
				} else {
					messages, err := s.store.MaildropSnapshot(context.Background(), session.user)
					if err != nil {
						s.store.UnlockMaildrop(session.user)
						reply = "-ERR mailbox unavailable"
					} else {
						session.authed = true
						session.recipient = session.user
						session.messages = messages
						session.deleted = make([]bool, len(messages))
						reply = fmt.Sprintf("+OK maildrop locked, %d message(s)", len(messages))
					}
				}
			case "QUIT":
				if arguments != "" {
					reply = "-ERR QUIT takes no arguments"
				} else {
					reply = "+OK"
					closeAfter = true
				}
			case "NOOP":
				reply = "+OK"
			default:
				reply = "-ERR command not recognized"
			}
		}

		if writePOP3(conn, s.config.ReadTimeout, reply) {
			return
		}
		if closeAfter {
			return
		}
	}
}

func (s *POP3Server) commitSession(session *pop3Session) string {
	ids := make([]string, 0)
	for i, deleted := range session.deleted {
		if deleted {
			ids = append(ids, session.messages[i].ID)
		}
	}
	if err := s.store.DeleteForRecipient(context.Background(), session.recipient, ids); err != nil {
		return "-ERR deletion failed"
	}
	return ""
}

func (s *POP3Server) writeMessage(conn net.Conn, recipient, id string) bool {
	stored, found, err := s.store.GetForRecipient(context.Background(), id, recipient, true)
	if err != nil || !found {
		return writePOP3(conn, s.config.ReadTimeout, "-ERR message unavailable")
	}
	if writePOP3(conn, s.config.ReadTimeout, fmt.Sprintf("+OK %d octets", len(stored.Raw))) {
		return true
	}
	if writeDotEncoded(conn, s.config.ReadTimeout, stored.Raw) {
		return true
	}
	return writePOP3(conn, s.config.ReadTimeout, ".")
}

func parsePOP3Command(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	verb, arguments, _ := strings.Cut(line, " ")
	return strings.ToUpper(verb), strings.TrimSpace(arguments)
}

func (s *pop3Session) stat() string {
	count, size := s.total()
	return fmt.Sprintf("+OK %d %d", count, size)
}

func (s *pop3Session) list(arguments string) string {
	if arguments == "" {
		count, size := s.total()
		var builder strings.Builder
		builder.WriteString(fmt.Sprintf("+OK %d messages (%d octets)\r\n", count, size))
		for i, message := range s.messages {
			if !s.deleted[i] {
				builder.WriteString(fmt.Sprintf("%d %d\r\n", i+1, message.Size))
			}
		}
		builder.WriteString(".")
		return builder.String()
	}
	index, ok := s.messageFor(arguments)
	if !ok {
		return "-ERR no such message"
	}
	return fmt.Sprintf("+OK %d %d", index+1, s.messages[index].Size)
}

func (s *pop3Session) uidl(arguments string) string {
	if arguments == "" {
		count, _ := s.total()
		var builder strings.Builder
		builder.WriteString(fmt.Sprintf("+OK %d messages\r\n", count))
		for i, message := range s.messages {
			if !s.deleted[i] {
				builder.WriteString(fmt.Sprintf("%d %s\r\n", i+1, message.ID))
			}
		}
		builder.WriteString(".")
		return builder.String()
	}
	index, ok := s.messageFor(arguments)
	if !ok {
		return "-ERR no such message"
	}
	return fmt.Sprintf("+OK %d %s", index+1, s.messages[index].ID)
}

func (s *pop3Session) messageFor(arguments string) (int, bool) {
	if arguments == "" || strings.ContainsFunc(arguments, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}
	number, err := strconv.Atoi(arguments)
	if err != nil || number < 1 || number > len(s.messages) || s.deleted[number-1] {
		return 0, false
	}
	return number - 1, true
}

func (s *pop3Session) total() (int, int) {
	count := 0
	size := 0
	for i, message := range s.messages {
		if !s.deleted[i] {
			count++
			size += message.Size
		}
	}
	return count, size
}

func writePOP3(conn net.Conn, timeout time.Duration, reply string) bool {
	return writeReply(conn, timeout, reply)
}

func writeDotEncoded(conn net.Conn, timeout time.Duration, raw []byte) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	lines := bytes.SplitAfter(raw, []byte("\r\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte(".")) {
			if _, err := conn.Write([]byte(".")); err != nil {
				return true
			}
		}
		if _, err := conn.Write(line); err != nil {
			return true
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			if _, err := conn.Write([]byte("\r\n")); err != nil {
				return true
			}
		}
	}
	return false
}
