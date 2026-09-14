package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/emersion/go-imap"

	"slices"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	jmap "github.com/naust-mail/naust-jmap/core/jmap"
	jbackend "github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
)

// Outbound Test

// The fake relay requires authentication, rejects temporarily/permanently on
// demand, and only counts a delivery after receiving the complete DATA body.
type fakeRelay struct {
	code     atomic.Int32
	received atomic.Int32
	workers  sync.WaitGroup
}

func newFakeRelay(t *testing.T, implicit bool) *fakeRelay {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	cfg := certServer.TLS.Clone()
	certServer.Close()
	ca := "relay-ca.pem"
	checkError(t, objects.Put(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cfg.Certificates[0].Certificate[0]})))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	checkError(t, err)
	if implicit {
		listener = tls.NewListener(listener, cfg)
	}
	relay := new(fakeRelay)
	relay.code.Store(250)
	config.RelayAddr = listener.Addr().String()
	config.RelayUser, config.RelayPassword = "relay-user", "relay-password"
	config.RelayPasswordFile = ""
	config.RelayTLS, config.RelayCAFile = "starttls", ca
	config.RelayRootCAs = x509.NewCertPool()
	if implicit {
		config.RelayTLS = "implicit"
	}
	config.RelayRootCAs.AddCert(certServer.Certificate())
	server := smtp.NewServer(relay)
	server.Domain, server.TLSConfig = "test-relay", cfg
	server.ReadTimeout, server.WriteTimeout = 5*time.Second, 5*time.Second
	relay.workers.Add(1)
	go func() { defer relay.workers.Done(); server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); relay.workers.Wait() })
	return relay
}
func (r *fakeRelay) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &relaySession{relay: r}, nil
}

type relaySession struct {
	relay         *fakeRelay
	authenticated bool
}

func (*relaySession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *relaySession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, smtp.ErrAuthUnsupported
	}
	return sasl.NewPlainServer(func(identity, user, password string) error {
		if identity != "" || user != "relay-user" || password != "relay-password" {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}
func (s *relaySession) Mail(string, *smtp.MailOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	return nil
}
func (s *relaySession) Rcpt(string, *smtp.RcptOptions) error {
	if code := int(s.relay.code.Load()); code != 250 {
		return &smtp.SMTPError{Code: code, Message: "recipient status"}
	}
	return nil
}
func (s *relaySession) Data(r io.Reader) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	s.relay.received.Add(1)
	return nil
}
func (*relaySession) Reset()        {}
func (*relaySession) Logout() error { return nil }
func outboundTestDir(t *testing.T) {
	t.Helper()
	oldConfig, oldObjects := config, objects
	config = defaultConfig()
	config.OutboundMode = "relay"
	config.QueueRetry = time.Millisecond
	objects = &memoryObjects{data: make(map[string][]byte)}
	if endpoint := os.Getenv("TEST_S3_ENDPOINT"); endpoint != "" {
		c := S3Config{Endpoint: endpoint, Bucket: os.Getenv("TEST_S3_BUCKET"), Region: "us-east-1", AccessKey: "test", SecretKey: "test"}
		bucket, err := connectBucket(c)
		checkError(t, err)
		isolated := &prefixedObjects{base: bucket, prefix: fmt.Sprintf("test-%d/", time.Now().UnixNano())}
		objects = isolated
		t.Cleanup(func() {
			keys, err := isolated.List("")
			checkError(t, err)
			for _, key := range keys {
				checkError(t, isolated.Delete(key))
			}
		})
	}
	t.Cleanup(func() { config = oldConfig; objects = oldObjects })
}
func queuedJob(t *testing.T) (string, *outboundJob) {
	t.Helper()
	paths, err := objects.List("t12e.cc/" + outbox + "/")
	var jobs []string
	for _, key := range paths {
		if strings.HasSuffix(key, ".json") {
			jobs = append(jobs, key)
		}
	}
	paths = jobs
	if err != nil || len(paths) != 1 {
		t.Fatalf("queue: %v %v", paths, err)
	}
	job, err := readJSON[*outboundJob](paths[0])
	checkError(t, err)
	return paths[0], job
}
func TestOutboundRetryAndRestart(t *testing.T) {
	outboundTestDir(t)
	relay := newFakeRelay(t, true)
	body := []byte("From: jw238@t12e.cc\r\nTo: recipient@example.net\r\nSubject: retry\r\n\r\nbody\r\n")
	checkError(t, queueMail("t12e.cc/jw238", "jw238@t12e.cc", nil, []string{"recipient@example.net"}, body))
	path, job := queuedJob(t)
	if string(job.Body) != string(body) {
		t.Fatal("body was not preserved")
	}
	relay.code.Store(451)
	claimAndProcess(t, path)
	_, job = queuedJob(t) // Reload disk state as a restarted worker would.
	if job.Recipients[0].State != "pending" || job.Recipients[0].Attempts != 1 || job.Recipients[0].Error == "" {
		t.Fatalf("temporary failure lost: %+v", job.Recipients)
	}
	job.Recipients[0].Next = time.Time{}
	checkError(t, writeJSON(path, job))
	relay.code.Store(250)
	claimAndProcess(t, path)
	if _, err := objects.Get(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("completed task/preclaim remained", err)
	}
	if relay.received.Load() != 1 {
		t.Fatal("retry delivery missing")
	}

}
func TestOutboundPermanentFailureNotifiesSender(t *testing.T) {
	outboundTestDir(t)
	relay := newFakeRelay(t, true)
	relay.code.Store(550)
	checkError(t, queueMail("t12e.cc/jw238", "jw238@t12e.cc", nil, []string{"missing@example.net"}, []byte("Subject: test\r\n\r\nbody\r\n")))
	path, _ := queuedJob(t)
	claimAndProcess(t, path)
	if _, err := objects.Get(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed terminal task/preclaim remained", err)
	}
	notices, err := messages("t12e.cc/jw238")
	if err != nil || len(notices) != 1 || !strings.Contains(string(notices[0].Body), "missing@example.net") {
		t.Fatal("expected one local failure notice", err)
	}
}
func TestExternalRecipientRequiresAuthentication(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/jw238/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/jw238/.password", []byte("123123")))
	session := &smtpSession{}
	if session.Rcpt("recipient@example.net", nil) == nil {
		t.Fatal("open relay")
	}
	session.user = "t12e.cc/jw238"
	checkError(t, session.Mail("jw238@t12e.cc", nil))
	checkError(t, session.Rcpt("recipient@example.net", nil))
	if err := session.Rcpt("unknown@t12e.cc", nil); err == nil {
		t.Fatal("unknown local recipient sent externally")
	}
	session.Reset()
	if len(session.remote) != 0 || session.from != "" || session.user != "t12e.cc/jw238" {
		t.Fatal("RSET leaked envelope or cleared authentication")
	}
	config.OutboundMode = "disabled"
	if err := session.Rcpt("recipient@example.net", nil); err == nil {
		t.Fatal("unconfigured transport accepted mail")
	}
}
func TestOutboundRejectsUntrustedTLS(t *testing.T) {
	outboundTestDir(t)
	newFakeRelay(t, true)
	config.RelayRootCAs = nil
	err := sendRemote(context.Background(), &outboundJob{From: "jw238@t12e.cc"}, "recipient@example.net")
	if err == nil {
		t.Fatal("untrusted TLS was accepted")
	}
}
func TestOutboundCancellation(t *testing.T) {
	outboundTestDir(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	checkError(t, err)
	defer listener.Close()
	config.RelayAddr = listener.Addr().String()
	config.RelayTLS = "starttls"
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, bufio.NewReader(c))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = sendRemote(ctx, &outboundJob{From: "jw238@t12e.cc"}, "recipient@example.net")
	if err == nil || time.Since(start) > time.Second {
		t.Fatal(fmt.Sprint("shutdown failed: ", err))
	}
	<-done
}

func TestOutboundRelaySTARTTLS(t *testing.T) {
	outboundTestDir(t)
	relay := newFakeRelay(t, false)
	job := &outboundJob{From: "jw238@t12e.cc", Body: []byte("Subject: STARTTLS\r\n\r\nbody\r\n")}
	checkError(t, sendRemote(context.Background(), job, "recipient@example.net"))
	if relay.received.Load() != 1 {
		t.Fatal("STARTTLS delivery missing")
	}
}

func TestSentArchiveDoesNotResurrectDeletedMail(t *testing.T) {
	outboundTestDir(t)
	checkError(t, queueMail("t12e.cc/jw238", "jw238@t12e.cc", nil, []string{"recipient@example.net"}, []byte("Subject: saved\r\n\r\nbody\r\n")))
	saved, err := messages(testFolderKey(t, "t12e.cc/jw238", "Sent"))
	if err != nil || len(saved) != 1 {
		t.Fatal("Sent missing", err)
	}
	checkError(t, deleteMessages(testFolderKey(t, "t12e.cc/jw238", "Sent"), []uint32{saved[0].Uid}))
	_, job := queuedJob(t)
	if !job.Archived {
		t.Fatal("Sent archive status not saved in task")
	}
	saved, err = messages(testFolderKey(t, "t12e.cc/jw238", "Sent"))
	if err != nil || len(saved) != 0 {
		t.Fatal("deleted Sent message resurrected", err)
	}
}

// Auth Login Test

func TestLoginExchange(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, password := range []string{"valid", "wrong", ""} {
			calls := 0
			denied := errors.New("denied")
			s := &loginServer{authenticate: func(u, p string) error {
				calls++
				if u != "user@example.org" || p != "valid" {
					return denied
				}
				return nil
			}}
			if !initial {
				challenge, done, err := s.Next(nil)
				if string(challenge) != "Username:" || done || err != nil {
					t.Fatalf("unexpected username challenge: %q %t %v", challenge, done, err)
				}
			}
			challenge, done, err := s.Next([]byte("user@example.org"))
			if string(challenge) != "Password:" || done || err != nil || calls != 0 {
				t.Fatalf("unexpected password challenge: %q %t %v", challenge, done, err)
			}
			_, done, err = s.Next([]byte(password))
			if !done || calls != 1 || (err == nil) != (password == "valid") {
				t.Fatalf("unexpected authentication result: %t %v calls=%d", done, err, calls)
			}
			if _, _, err = s.Next([]byte("valid")); err == nil || calls != 1 {
				t.Fatal("completed exchange was reused")
			}
		}
	}
}

// Configuration is assembled before serving and is not reloaded per message.
func TestConfigDefaultsAndFlagPrecedence(t *testing.T) {
	calls := map[string]int{}
	c, err := loadConfig([]string{"-outbound", "direct", "-queue-retry", "2s", "-smtps", "127.0.0.1:2465"}, func(key string) string {
		calls[key]++
		if key == "FMA_OUTBOUND_MODE" {
			return "relay"
		}
		return ""
	})
	checkError(t, err)
	if c.OutboundMode != "direct" || c.QueueRetry != 2*time.Second || c.SMTPSAddr != "127.0.0.1:2465" || c.POP3SAddr != "127.0.0.1:1995" {
		t.Fatal("configuration defaults or flag precedence changed")
	}
	if len(calls) != 15 {
		t.Fatalf("expected 15 environment inputs, got %d", len(calls))
	}
	for key, count := range calls {
		if count != 1 {
			t.Fatalf("%s read %d times", key, count)
		}
	}
}

func TestConfigRelaySnapshot(t *testing.T) {
	outboundTestDir(t)
	relay := newFakeRelay(t, true)
	passwordPath := "password"
	checkError(t, objects.Put(passwordPath, []byte("relay-password\r\n")))
	env := map[string]string{
		"FMA_OUTBOUND_MODE": "relay", "FMA_RELAY_ADDR": config.RelayAddr,
		"FMA_RELAY_USER": "relay-user", "FMA_RELAY_PASSWORD": "overridden-password",
		"FMA_RELAY_PASSWORD_FILE": "password", "FMA_RELAY_CA_FILE": config.RelayCAFile, "FMA_RELAY_TLS": "implicit",
	}
	loaded, err := loadConfig(nil, func(key string) string { return env[key] })
	checkError(t, err)
	checkError(t, loadRelayObjects(&loaded))
	if loaded.RelayPassword != "relay-password" || loaded.RelayRootCAs == nil {
		t.Fatal("secret file or CA not loaded")
	}
	checkError(t, objects.Delete(passwordPath))
	checkError(t, objects.Delete(config.RelayCAFile))
	for key := range env {
		t.Setenv(key, "invalid-after-startup")
	}
	config = loaded
	checkError(t, sendRemote(context.Background(), &outboundJob{From: "jw238@t12e.cc", Body: []byte("Subject: snapshot\r\n\r\nbody\r\n")}, "recipient@example.net"))
	if relay.received.Load() != 1 {
		t.Fatal("snapshot did not deliver")
	}
}

func TestConfigValidationAndQueue(t *testing.T) {
	loadConfig := func(args []string, env func(string) string) (Config, error) {
		c, err := loadConfig(args, env)
		if err == nil {
			c.S3 = S3Config{Bucket: "test", Region: "us-east-1", AccessKey: "test", SecretKey: "test"}
			err = checkConfig(c)
		}
		return c, err
	}

	outboundTestDir(t)
	base := map[string]string{"FMA_OUTBOUND_MODE": "relay", "FMA_RELAY_ADDR": "smtp.example.org:587", "FMA_RELAY_USER": "user", "FMA_RELAY_PASSWORD": "secret"}
	for _, tc := range []struct{ key, value string }{
		{"FMA_OUTBOUND_MODE", "invalid"}, {"FMA_RELAY_ADDR", "invalid"},
		{"FMA_RELAY_USER", ""}, {"FMA_RELAY_PASSWORD", ""},
		{"FMA_RELAY_TLS", "plaintext"}, {"FMA_RELAY_PASSWORD_FILE", "missing-secret"}, {"FMA_RELAY_CA_FILE", "missing-ca"},
	} {
		_, err := loadConfig(nil, func(key string) string {
			if key == tc.key {
				return tc.value
			}
			return base[key]
		})
		if err == nil {
			loaded, e := loadConfig(nil, func(key string) string {
				if key == tc.key {
					return tc.value
				}
				return base[key]
			})
			if e == nil {
				err = loadRelayObjects(&loaded)
			} else {
				err = e
			}
		}
		if err == nil {
			t.Fatalf("invalid %s accepted", tc.key)
		}
	}
	checkError(t, objects.Put("bad-ca", []byte("not a certificate")))
	loaded, err := loadConfig(nil, func(key string) string {
		if key == "FMA_RELAY_CA_FILE" {
			return "bad-ca"
		}
		return base[key]
	})
	checkError(t, err)
	if loadRelayObjects(&loaded) == nil {
		t.Fatal("invalid CA accepted")
	}

	for _, retry := range []string{"0s", "-1s"} {
		if _, err := loadConfig([]string{"-queue-retry", retry}, func(string) string { return "" }); err == nil {
			t.Fatal("nonpositive retry accepted")
		}
	}
	if _, err := loadConfig([]string{"-queue"}, func(key string) string {
		if key == "FMA_OUTBOUND_MODE" {
			return "relay"
		}
		return ""
	}); err != nil {
		t.Fatal("queue inspection required relay config", err)
	}

	c, err := loadConfig(nil, func(key string) string { return base[key] })
	if err != nil || c.RelayTLS != "starttls" {
		t.Fatal("default relay TLS changed", err)
	}
}

func TestSharedLocalAndQueuedAcceptance(t *testing.T) {
	outboundTestDir(t)
	local := []byte("Subject: local\r\n\r\nlocal body\r\n")
	remote := []byte("Subject: remote\r\n\r\nremote body\r\n")
	checkError(t, queueMail("t12e.cc/jw238", "jw238@t12e.cc", []string{"t12e.cc/jw238"}, nil, local))
	checkError(t, queueMail("t12e.cc/jw238", "jw238@t12e.cc", []string{"t12e.cc/jw238"}, []string{"recipient@example.net"}, remote))
	for _, key := range []string{"t12e.cc/jw238", testFolderKey(t, "t12e.cc/jw238", "Sent")} {
		mail, err := messages(key)
		if err != nil || len(mail) != 2 || string(mail[0].Body) != string(local) || string(mail[1].Body) != string(remote) {
			t.Fatalf("shared storage %s: count=%d err=%v", key, len(mail), err)
		}
	}
	_, job := queuedJob(t)
	if len(job.Recipients) != 1 || job.Recipients[0].State != "pending" || string(job.Body) != string(remote) {
		t.Fatal("queue lost recipient state or original body")
	}
	checkError(t, saveSent("t12e.cc/jw238", remote))
	sent, err := messages(testFolderKey(t, "t12e.cc/jw238", "Sent"))
	if err != nil || len(sent) != 2 {
		t.Fatal("Sent deduplication failed", err)
	}
}

func checkError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The in-memory store exists only in tests; production always connects to S3.
type memoryObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memoryObjects) Get(k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[k]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return bytes.Clone(b), nil
}
func (m *memoryObjects) Put(k string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = bytes.Clone(b)
	return nil
}
func (m *memoryObjects) Create(k string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[k]; ok {
		return fs.ErrExist
	}
	m.data[k] = bytes.Clone(b)
	return nil
}
func (m *memoryObjects) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}
func (m *memoryObjects) List(prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

type prefixedObjects struct {
	base   objectStore
	prefix string
}

func (p *prefixedObjects) Get(k string) ([]byte, error)    { return p.base.Get(p.prefix + k) }
func (p *prefixedObjects) Put(k string, b []byte) error    { return p.base.Put(p.prefix+k, b) }
func (p *prefixedObjects) Create(k string, b []byte) error { return p.base.Create(p.prefix+k, b) }
func (p *prefixedObjects) Delete(k string) error           { return p.base.Delete(p.prefix + k) }
func (p *prefixedObjects) List(prefix string) ([]string, error) {
	keys, err := p.base.List(p.prefix + prefix)
	for i := range keys {
		keys[i] = strings.TrimPrefix(keys[i], p.prefix)
	}
	return keys, err
}
func testFolderKey(t *testing.T, user, name string) string {
	t.Helper()
	meta, err := getFolder(user, name)
	checkError(t, err)
	return meta.Key
}

func TestBucketLockExcludesConcurrentWriters(t *testing.T) {
	outboundTestDir(t)
	store := objects
	var wg sync.WaitGroup
	var winners atomic.Int32
	start := make(chan struct{})
	releases := make(chan *bucketLease, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := lockBucket(store, time.Now(), "t12e.cc")
			if err == nil {
				winners.Add(1)
				releases <- release
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	if winners.Load() != 1 {
		t.Fatalf("expected one writer, got %d", winners.Load())
	}
	for release := range releases {
		release.release(time.Now())
	}
	release, err := lockBucket(store, time.Now(), "t12e.cc")
	checkError(t, err)
	release.release(time.Now())
}
func TestFoldersRenameAndRecreate(t *testing.T) {
	outboundTestDir(t)
	user := &imapUser{name: "t12e.cc/alice"}
	checkError(t, user.CreateMailbox("Work"))
	box, err := user.GetMailbox("Work")
	checkError(t, err)
	b := box.(*inbox)
	body := []byte("Subject: test\r\n\r\nhello\r\n")
	checkError(t, b.CreateMessage(nil, time.Time{}, bytes.NewReader(body)))
	checkError(t, b.Poll())
	oldKey, oldUID := b.key(), b.meta.Validity
	checkError(t, user.RenameMailbox("Work", "Projects"))
	checkError(t, b.Poll())
	if b.Name() != "Projects" || len(b.Messages) != 1 || b.key() != oldKey {
		t.Fatal("rename lost selected folder identity")
	}
	reloaded, err := user.GetMailbox("Projects")
	checkError(t, err)
	if reloaded.(*inbox).meta.Validity != oldUID {
		t.Fatal("rename changed UIDVALIDITY")
	}
	checkError(t, user.DeleteMailbox("Projects"))
	checkError(t, user.CreateMailbox("Projects"))
	reloaded, err = user.GetMailbox("Projects")
	checkError(t, err)
	if reloaded.(*inbox).key() == oldKey || len(reloaded.(*inbox).Messages) != 0 {
		t.Fatal("deleted data resurrected")
	}
	if b.CreateMessage(nil, time.Time{}, bytes.NewReader(body)) == nil {
		t.Fatal("stale handle wrote deleted mailbox")
	}
}
func TestS3StorageReloadAndPagination(t *testing.T) {
	outboundTestDir(t)
	if authenticate("alice@t12e.cc", "secret") {
		t.Fatal("missing account accepted")
	}
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	if !authenticate("alice@t12e.cc", "secret") {
		t.Fatal("external account was not discovered")
	}

	checkError(t, deliver([]string{"t12e.cc/alice"}, []byte("Subject: inbox\r\n\r\nbody")))
	checkError(t, saveSent("t12e.cc/alice", []byte("Subject: sent\r\n\r\nbody")))
	inboxMessages, err := messages("t12e.cc/alice")
	checkError(t, err)
	if len(inboxMessages) != 1 {
		t.Fatal("recursive listing included folder or metadata objects")
	}
	// Exercise real continuation tokens when TEST_S3_ENDPOINT is configured.
	for i := 0; i < 1005; i++ {
		checkError(t, objects.Put(fmt.Sprintf("pages/%04d.json", i), []byte(`1`)))
	}
	count := 0
	checkError(t, eachJSON("pages", func(_ string, n int, err error) error {
		if n != 1 {
			t.Errorf("invalid value %d", n)
		}
		count++
		return err
	}))
	if count != 1005 {
		t.Fatalf("pagination lost objects: %d", count)
	}
}
func TestMissingOrUnavailableBucket(t *testing.T) {
	if err := checkConfig(defaultConfig()); err == nil {
		t.Fatal("missing bucket accepted")
	}
	server := httptest.NewServer(nil)
	server.Close()
	if _, err := connectBucket(S3Config{Endpoint: server.URL, Bucket: "missing", AccessKey: "x", SecretKey: "y"}); err == nil {
		t.Fatal("unavailable backend accepted")
	}
}
func TestFolderFlagsPersist(t *testing.T) {
	outboundTestDir(t)
	user := &imapUser{name: "t12e.cc/alice"}
	checkError(t, user.CreateMailbox("Test"))
	box, err := user.GetMailbox("Test")
	checkError(t, err)
	b := box.(*inbox)
	checkError(t, b.CreateMessage(nil, time.Time{}, bytes.NewReader([]byte("Subject: flags\r\n\r\nbody"))))
	checkError(t, b.Poll())
	set := new(imap.SeqSet)
	set.AddNum(1)
	checkError(t, b.UpdateMessagesFlags(false, set, imap.AddFlags, []string{imap.SeenFlag}))
	fresh, err := user.GetMailbox("Test")
	checkError(t, err)
	if len(fresh.(*inbox).Messages[0].Flags) != 1 {
		t.Fatal("flags were not persisted")
	}
}

type blockingObjects struct {
	objectStore
	entered chan struct{}
	resume  chan struct{}
}

func (s *blockingObjects) Put(k string, data []byte) error {
	close(s.entered)
	<-s.resume
	return s.objectStore.Put(k, data)
}
func TestStorageShutdownDrainsWrites(t *testing.T) {
	outboundTestDir(t)
	slow := &blockingObjects{objectStore: objects, entered: make(chan struct{}), resume: make(chan struct{})}
	guard := &guardedStore{base: slow}
	written := make(chan error, 1)
	go func() { written <- guard.Put("pending", []byte("complete")) }()
	<-slow.entered
	closed := make(chan struct{})
	go func() { guard.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("shutdown abandoned an active write")
	case <-time.After(10 * time.Millisecond):
	}
	close(slow.resume)
	checkError(t, <-written)
	<-closed
	if !errors.Is(guard.Put("later", nil), net.ErrClosed) {
		t.Fatal("write accepted after shutdown")
	}
	if _, err := objects.Get("later"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("late object persisted")
	}
	data, err := objects.Get("pending")
	checkError(t, err)
	if string(data) != "complete" {
		t.Fatal("pending write lost")
	}
}

func (m *memoryObjects) GetVersion(k string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[k]
	if !ok {
		return nil, "", fs.ErrNotExist
	}
	return bytes.Clone(data), fmt.Sprintf("\"%x\"", sha256.Sum256(data)), nil
}
func (m *memoryObjects) Swap(k string, data []byte, etag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.data[k]
	if !ok || fmt.Sprintf("\"%x\"", sha256.Sum256(old)) != etag {
		return fs.ErrExist
	}
	m.data[k] = bytes.Clone(data)
	return nil
}
func (p *prefixedObjects) GetVersion(k string) ([]byte, string, error) {
	return p.base.(versionedStore).GetVersion(p.prefix + k)
}
func (p *prefixedObjects) Swap(k string, b []byte, etag string) error {
	return p.base.(versionedStore).Swap(p.prefix+k, b, etag)
}

func TestLeaseRenewalExpiryAndStaleRelease(t *testing.T) {
	outboundTestDir(t)
	now := time.Now().UTC()
	first, err := lockBucket(objects, now, "t12e.cc")
	checkError(t, err)
	checkError(t, first.renew(now.Add(15*time.Second)))
	record, _, err := readLock(objects.(versionedStore), "t12e.cc")
	checkError(t, err)
	if !record.StartedAt.Equal(now) || !record.RenewedAt.Equal(now.Add(15*time.Second)) || !record.ExpiresAt.Equal(now.Add(45*time.Second)) {
		t.Fatal("incorrect lease timestamps")
	}
	if _, err := lockBucket(objects, now.Add(31*time.Second), "t12e.cc"); err == nil {
		t.Fatal("renewed lease stolen")
	}
	second, err := lockBucket(objects, now.Add(46*time.Second), "t12e.cc")
	checkError(t, err)
	if first.renew(now.Add(47*time.Second)) == nil {
		t.Fatal("expired owner renewed")
	}
	first.release(now.Add(47 * time.Second))
	record, _, err = readLock(objects.(versionedStore), "t12e.cc")
	checkError(t, err)
	if record.Owner != second.record.Owner {
		t.Fatal("stale release changed successor lock")
	}
	second.release(now.Add(47 * time.Second))
	third, err := lockBucket(objects, now.Add(48*time.Second), "t12e.cc")
	checkError(t, err)
	third.release(now.Add(49 * time.Second))
}
func TestExpiredLeaseTakeoverRace(t *testing.T) {
	outboundTestDir(t)
	now := time.Now()
	_, err := lockBucket(objects, now.Add(-time.Minute), "t12e.cc")
	checkError(t, err)
	var wg sync.WaitGroup
	var winners atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := lockBucket(objects, now, "t12e.cc"); err == nil {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("expired lease acquired by %d writers", winners.Load())
	}
}
func TestLeaseLossStopsWriter(t *testing.T) {
	outboundTestDir(t)
	lease, err := lockBucket(objects, time.Now(), "t12e.cc")
	checkError(t, err)
	guard := &guardedStore{base: objects, lease: lease}
	lease.mu.Lock()
	lease.record.ExpiresAt = time.Now().Add(-time.Second)
	lease.mu.Unlock()
	if guard.Put("after-expiry", []byte("bad")) == nil {
		t.Fatal("expired writer accepted storage operation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := lease.keepAlive(ctx, cancel)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expired lease did not stop server")
	}
	<-done
	if lease.failure() == nil {
		t.Fatal("lease loss not reported")
	}
}

func TestExternalAccountsWithoutReload(t *testing.T) {
	outboundTestDir(t)
	if authenticate("alice@t12e.cc", "secret") {
		t.Fatal("missing account accepted")
	}
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret\n")))
	if !authenticate("alice@t12e.cc", "secret") || authenticate("alice@t12e.cc", "wrong") {
		t.Fatal("new account authentication failed")
	}
	session := &smtpSession{}
	checkError(t, session.Rcpt("alice@t12e.cc", nil))
	if session.Rcpt("alicex@t12e.cc", nil) == nil {
		t.Fatal("prefix isolation failed")
	}
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("changed")))
	if authenticate("alice@t12e.cc", "secret") || !authenticate("alice@t12e.cc", "changed") {
		t.Fatal("password change required reload")
	}
	checkError(t, objects.Delete("t12e.cc/alice/.password"))
	if authenticate("alice@t12e.cc", "changed") || session.Rcpt("alice@t12e.cc", nil) == nil {
		t.Fatal("deleted account still accepted")
	}
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", nil))
	if authenticate("alice@t12e.cc", "") || session.Rcpt("alice@t12e.cc", nil) == nil {
		t.Fatal("empty password accepted")
	}
}

func claimAndProcess(t *testing.T, key string) {
	t.Helper()
	claim, err := preclaimTask(key, "test-worker", time.Now())
	checkError(t, err)
	if claim == nil {
		t.Fatal("task not claimed")
	}
	checkError(t, claim.execute(context.Background()))
}

func TestPreclaimRaceTimeoutAndStaleCompletion(t *testing.T) {
	outboundTestDir(t)
	checkError(t, queueMail("t12e.cc/alice", "alice@t12e.cc", nil, []string{"r@example.net"}, []byte("Subject: claim\r\n\r\nbody")))
	key, _ := queuedJob(t)
	var wg sync.WaitGroup
	winners := make(chan *claimedTask, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := preclaimTask(key, "node", time.Now())
			if err == nil && c != nil {
				winners <- c
			}
		}()
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("%d claim winners", len(winners))
	}
	old := <-winners
	if c, err := preclaimTask(key, "t12e.cc/other", old.expires.Add(-time.Nanosecond)); err != nil || c != nil {
		t.Fatal("live claim stolen", err)
	}
	current, err := preclaimTask(key, "t12e.cc/other", old.expires)
	checkError(t, err)
	if current == nil || current.job.Preclaim.Owner != "t12e.cc/other" {
		t.Fatal("expired preclaim not recovered")
	}
	old.job.Complete = true
	old.job.Preclaim = nil
	if old.save(context.Background()) == nil {
		t.Fatal("old worker overwrote successor")
	}
	stored, err := readJSON[*outboundJob](key)
	checkError(t, err)
	if stored.Complete || stored.Preclaim.Owner != "t12e.cc/other" {
		t.Fatal("successor lost")
	}
}

type failTaskDelete struct {
	objectStore
	versionedStore
	fail bool
}

func (s *failTaskDelete) Delete(k string) error {
	if s.fail && strings.HasSuffix(k, ".json") {
		return fmt.Errorf("injected delete failure")
	}
	return s.objectStore.Delete(k)
}
func TestCompletedTaskCleanupAfterCrash(t *testing.T) {
	outboundTestDir(t)
	relay := newFakeRelay(t, true)
	checkError(t, queueMail("t12e.cc/alice", "alice@t12e.cc", nil, []string{"r@example.net"}, []byte("Subject: cleanup\r\n\r\nbody")))
	key, _ := queuedJob(t)
	base := objects
	fault := &failTaskDelete{objectStore: base, versionedStore: base.(versionedStore), fail: true}
	objects = fault
	c, err := preclaimTask(key, "node", time.Now())
	checkError(t, err)
	if c.execute(context.Background()) == nil {
		t.Fatal("delete failure hidden")
	}
	job, err := readJSON[*outboundJob](key)
	checkError(t, err)
	if !job.Complete || job.Preclaim != nil {
		t.Fatal("completion was not committed")
	}
	fault.fail = false
	c, err = preclaimTask(key, "replacement", time.Now())
	checkError(t, err)
	if c != nil || relay.received.Load() != 1 {
		t.Fatal("completed task resent")
	}
	if _, err := objects.Get(key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("cleanup did not remove task", err)
	}
}

func TestConcurrentS3MailboxAllocation(t *testing.T) {
	outboundTestDir(t)
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := ensureFolders("t12e.cc/alice"); err != nil {
				failures <- err
				return
			}
			failures <- appendMessage("t12e.cc/alice", fmt.Appendf(nil, "message %d", i), nil, time.Now(), false)
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		checkError(t, err)
	}
	mail, err := messages("t12e.cc/alice")
	checkError(t, err)
	if len(mail) != 16 {
		t.Fatalf("concurrent delivery lost messages: %d", len(mail))
	}
}

func TestScannerYieldsAt1024WithoutDroppingClaims(t *testing.T) {
	outboundTestDir(t)
	// This capacity test needs no S3 latency; CAS correctness is exercised on
	// both backends by the takeover tests above.
	objects = &memoryObjects{data: make(map[string][]byte)}
	for i := 0; i < maxClaimedTasks+1; i++ {
		key := fmt.Sprintf("%s/%032x.json", "t12e.cc/"+outbox, i)
		checkError(t, writeJSON(key, &outboundJob{ID: fmt.Sprintf("%032x", i), Archived: true, Recipients: []outboundRecipient{{Address: "a@example.net", State: "pending"}}}))
	}
	lease, err := lockBucket(objects, time.Now(), "t12e.cc")
	checkError(t, err)
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxClaimedTasks)
	gate := make(chan struct{})
	defer func() { close(gate); wg.Wait() }()
	full, err := scanTasks(context.Background(), lease, slots, &wg, func(c *claimedTask) error { <-gate; return nil })
	checkError(t, err)
	if !full || len(slots) != maxClaimedTasks {
		t.Fatalf("incorrect batch: full=%v active=%d", full, len(slots))
	}
	lease.release(time.Now())
	next, err := lockBucket(objects, time.Now(), "t12e.cc")
	checkError(t, err)
	defer next.release(time.Now())
	key := fmt.Sprintf("%s/%032x.json", "t12e.cc/"+outbox, maxClaimedTasks)
	last, err := preclaimTask(key, next.owner(), time.Now())
	checkError(t, err)
	if last == nil {
		t.Fatal("next node could not claim remaining task")
	}
	first, err := preclaimTask(fmt.Sprintf("%s/%032x.json", "t12e.cc/"+outbox, 0), next.owner(), time.Now())
	checkError(t, err)
	if first != nil {
		t.Fatal("releasing scanner lock released a live task")
	}
}

func TestPreclaimExecutionCancellationPreservesTask(t *testing.T) {
	outboundTestDir(t)
	checkError(t, queueMail("t12e.cc/alice", "alice@t12e.cc", nil, []string{"a@example.net"}, []byte("Subject: cancel\r\n\r\nbody")))
	key, _ := queuedJob(t)
	c, err := preclaimTask(key, "node", time.Now())
	checkError(t, err)
	if c.job.Preclaim.ExpiresAt.Sub(c.job.Preclaim.StartedAt) != 20*time.Second {
		t.Fatal("incorrect timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.execute(ctx)
	job, err := readJSON[*outboundJob](key)
	checkError(t, err)
	if job.Complete || job.Preclaim == nil || job.Recipients[0].State != "pending" {
		t.Fatal("cancelled task lost")
	}
}

func TestConfigEnvironmentPrefixAndOfflineVersion(t *testing.T) {
	loaded, err := loadConfig([]string{"--version"}, func(key string) string {
		if !strings.HasPrefix(key, "FMA_") {
			t.Fatalf("unprefixed environment lookup: %s", key)
		}
		if key == "FMA_OUTBOUND_MODE" {
			return "invalid-but-offline"
		}
		return ""
	})
	checkError(t, err)
	if !loaded.ShowVersion {
		t.Fatal("version flag not set")
	}
}

func TestAliasIdentityAndSharedMailbox(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	checkError(t, objects.Put("t12e.cc/sales/.kind", []byte("alias")))
	checkError(t, objects.Put("t12e.cc/sales/.alias", []byte("alice\n")))
	checkError(t, objects.Put("t12e.cc/sales/.password", []byte("ignored")))
	checkError(t, objects.Put("t12e.cc/alice/.profile.json", []byte(`{"display_name":"Alice","avatar":"avatar.png"}`)))
	identity, err := authenticateAccount("sales@t12e.cc", "secret")
	checkError(t, err)
	if identity.LoginID != "t12e.cc/sales" || identity.RootID != "t12e.cc/alice" || authenticate("sales@t12e.cc", "ignored") {
		t.Fatal("alias identity or credentials incorrect")
	}
	sender := &smtpSession{requireAuth: true}
	checkError(t, sender.authenticate("alice@t12e.cc", "sales@t12e.cc", "secret"))
	checkError(t, sender.Mail("sales@t12e.cc", nil))
	checkError(t, sender.Rcpt("alice@t12e.cc", nil))
	checkError(t, sender.Rcpt("sales@t12e.cc", nil))
	if len(sender.recipients) != 1 {
		t.Fatal("root and alias were not deduplicated")
	}
	checkError(t, sender.Data(strings.NewReader("Subject: aliases\r\n\r\nbody\r\n")))
	mailbox, err := messages("t12e.cc/alice")
	checkError(t, err)
	if len(mailbox) != 1 {
		t.Fatal("metadata treated as mail or duplicate delivery")
	}
	aliasMail, err := messages("t12e.cc/sales")
	checkError(t, err)
	if len(aliasMail) != 0 {
		t.Fatal("mail stored under alias prefix")
	}
	user, err := (imapBackend{}).Login(nil, "sales@t12e.cc", "secret")
	checkError(t, err)
	if user.Username() != "t12e.cc/sales" || user.(*imapUser).name != "t12e.cc/alice" {
		t.Fatal("IMAP lost login or root ID")
	}
	pop := &popMailbox{deleted: map[int]bool{}}
	checkError(t, pop.Login(context.Background(), "sales@t12e.cc", "secret"))
	defer pop.Close()
	second := &popMailbox{deleted: map[int]bool{}}
	if second.Login(context.Background(), "alice@t12e.cc", "secret") == nil {
		second.Close()
		t.Fatal("alias bypassed root maildrop lock")
	}
	if pop.loginID != "t12e.cc/sales" || pop.user != "t12e.cc/alice" {
		t.Fatal("POP identity incorrect")
	}
	checkError(t, pop.Dele(context.Background(), 1))
	checkError(t, pop.Close())
	mailbox, err = messages("t12e.cc/alice")
	checkError(t, err)
	if len(mailbox) != 1 {
		t.Fatal("disconnect committed alias deletes")
	}
	checkError(t, objects.Put("t12e.cc/other/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/other/.password", []byte("different")))
	checkError(t, objects.Put("t12e.cc/sales/.kind", []byte("alias")))
	checkError(t, objects.Put("t12e.cc/sales/.alias", []byte("other")))
	if authenticate("sales@t12e.cc", "secret") || !authenticate("sales@t12e.cc", "different") {
		t.Fatal("alias target change not immediate")
	}
	if sender.Mail("sales@t12e.cc", nil) == nil {
		t.Fatal("old session impersonated new alias owner")
	}
}

func TestInvalidAliasesFailClosed(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	for _, target := range []string{"", "missing", "../alice", "alice@t12e.cc", "t12e.cc/sales"} {
		checkError(t, objects.Put("t12e.cc/sales/.kind", []byte("alias")))
		checkError(t, objects.Put("t12e.cc/sales/.alias", []byte(target)))
		if authenticate("sales@t12e.cc", "secret") {
			t.Fatalf("invalid alias accepted: %q", target)
		}
		if (&smtpSession{}).Rcpt("sales@t12e.cc", nil) == nil {
			t.Fatal("invalid alias recipient accepted")
		}
	}
	checkError(t, objects.Put("t12e.cc/sales/.kind", []byte("alias")))
	checkError(t, objects.Put("t12e.cc/sales/.alias", []byte("support")))
	checkError(t, objects.Put("t12e.cc/support/.kind", []byte("alias")))
	checkError(t, objects.Put("t12e.cc/support/.alias", []byte("sales")))
	if authenticate("sales@t12e.cc", "secret") {
		t.Fatal("alias cycle accepted")
	}
}

func TestStartupConfigValidation(t *testing.T) {
	valid := defaultConfig()
	valid.S3 = S3Config{Bucket: "test", Region: "us-east-1", AccessKey: "key", SecretKey: "secret"}
	checkError(t, checkConfig(valid))
	for _, change := range []func(*Config){
		func(c *Config) { c.S3.Bucket = "" }, func(c *Config) { c.S3.Region = "" },
		func(c *Config) { c.S3.AccessKey = "" }, func(c *Config) { c.S3.SecretKey = "" },
		func(c *Config) { c.S3.Endpoint = "ftp://localhost" },
		func(c *Config) { c.CertFile = "" }, func(c *Config) { c.KeyFile = "" },
		func(c *Config) { c.SMTPAddr = "localhost:99999" }, func(c *Config) { c.IMAPAddr = c.SMTPAddr },
	} {
		c := valid
		change(&c)
		if checkConfig(c) == nil {
			t.Fatal("invalid startup configuration accepted")
		}
	}
	checkError(t, checkConfig(Config{ShowVersion: true}))
	c := valid
	c.ShowQueue = true
	c.OutboundMode = "invalid"
	checkError(t, checkConfig(c))
}

func TestLogLevelFiltering(t *testing.T) {
	old := logger
	t.Cleanup(func() { logger = old; slog.SetDefault(old) })
	for _, tc := range []struct {
		value string
		level slog.Level
	}{{"", slog.LevelInfo}, {"DEBUG", slog.LevelDebug}, {"info", slog.LevelInfo}, {"warn", slog.LevelWarn}, {"error", slog.LevelError}} {
		checkError(t, initLogger(tc.value))
		for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
			if logger.Enabled(context.Background(), level) != (level >= tc.level) {
				t.Fatalf("incorrect filtering for %q", tc.value)
			}
		}
	}
	if initLogger("invalid") == nil {
		t.Fatal("invalid log level accepted")
	}
}

func TestLogOutputStreams(t *testing.T) {
	var output, diagnostics bytes.Buffer
	l := newLogger(&output, &diagnostics, slog.LevelDebug).With("service", "fma").WithGroup("request")
	l.Debug("debug message")
	l.Info("ordinary message")
	l.Warn("warning message")
	l.Error("error message")
	if strings.Count(output.String(), "level=") != 3 || !strings.Contains(output.String(), "warning message") || !strings.Contains(output.String(), "ordinary message") {
		t.Fatal("incorrect stdout routing", output.String())
	}
	if strings.Count(diagnostics.String(), "level=") != 1 || strings.Contains(diagnostics.String(), "ordinary message") || !strings.Contains(diagnostics.String(), "error message") {
		t.Fatal("incorrect stderr routing", diagnostics.String())
	}
}

func TestMountedTLSSecret(t *testing.T) {
	old := objects
	objects = nil // Mounted credentials must not require a certificate read from S3.
	t.Cleanup(func() { objects = old })
	server := httptest.NewTLSServer(nil)
	cert := server.TLS.Certificates[0]
	server.Close()
	dir := t.TempDir()
	private, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	checkError(t, err)
	checkError(t, os.WriteFile(dir+"/tls.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0400))
	checkError(t, os.WriteFile(dir+"/tls.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0400))
	loaded, err := loadTLSCertificate(Config{TLSDir: dir})
	checkError(t, err)
	if !bytes.Equal(loaded.Certificate[0], cert.Certificate[0]) {
		t.Fatal("mounted certificate changed")
	}
	checkError(t, os.Remove(dir+"/tls.key"))
	if _, err := loadTLSCertificate(Config{TLSDir: dir}); err == nil {
		t.Fatal("missing mounted key accepted")
	}
	c := defaultConfig()
	c.S3 = S3Config{Bucket: "test", Region: "us-east-1", AccessKey: "key", SecretKey: "secret"}
	c.TLSDir = dir
	c.CertFile = ""
	c.KeyFile = ""
	checkError(t, checkConfig(c))
}

func TestIdentityKindSelectsConfiguration(t *testing.T) {
	outboundTestDir(t)
	for key, value := range map[string]string{
		"t12e.cc/slot/.password": "slot-password", "t12e.cc/slot/.alias": "root", "t12e.cc/slot/.proxy": "remote@example.net",
		"t12e.cc/root/.kind": "account", "t12e.cc/root/.password": "root-password",
	} {
		checkError(t, objects.Put(key, []byte(value)))
	}
	for _, kind := range []string{"", "unknown", "account alias", "ACCOUNT"} {
		checkError(t, objects.Put("t12e.cc/slot/.kind", []byte(kind)))
		if authenticate("slot@t12e.cc", "slot-password") || (&smtpSession{}).Rcpt("slot@t12e.cc", nil) == nil {
			t.Fatal("invalid kind accepted", kind)
		}
	}
	checkError(t, objects.Delete("t12e.cc/slot/.kind"))
	if authenticate("slot@t12e.cc", "slot-password") {
		t.Fatal("missing kind inferred from password")
	}
	checkError(t, objects.Put("t12e.cc/slot/.kind", []byte("account\n")))
	if !authenticate("slot@t12e.cc", "slot-password") || authenticate("slot@t12e.cc", "root-password") {
		t.Fatal("account used alias or proxy config")
	}
	account := &smtpSession{}
	checkError(t, account.Rcpt("slot@t12e.cc", nil))
	if len(account.remote) != 0 || len(account.recipients) != 1 || account.recipients[0] != "t12e.cc/slot" {
		t.Fatal("account routed via stale proxy")
	}
	checkError(t, objects.Put("t12e.cc/slot/.kind", []byte("alias")))
	if !authenticate("slot@t12e.cc", "root-password") || authenticate("slot@t12e.cc", "slot-password") {
		t.Fatal("alias used stale password")
	}
	checkError(t, objects.Put("t12e.cc/slot/.kind", []byte("proxy")))
	if authenticate("slot@t12e.cc", "slot-password") || authenticate("slot@t12e.cc", "root-password") {
		t.Fatal("proxy allowed authentication")
	}
	proxy := &smtpSession{}
	checkError(t, proxy.Rcpt("slot@t12e.cc", nil))
	if len(proxy.remote) != 1 || proxy.remote[0] != "remote@example.net" || len(proxy.recipients) != 0 {
		t.Fatal("proxy used stale account or alias")
	}
}

func TestProxyLocalRoutingAndCycles(t *testing.T) {
	outboundTestDir(t)
	config.OutboundMode = "disabled"
	for key, value := range map[string]string{
		"t12e.cc/forward/.kind": "proxy", "t12e.cc/forward/.proxy": "alias@t12e.cc",
		"t12e.cc/alias/.kind": "alias", "t12e.cc/alias/.alias": "root",
		"t12e.cc/root/.kind": "account", "t12e.cc/root/.password": "secret",
	} {
		checkError(t, objects.Put(key, []byte(value)))
	}
	body := []byte("From: sender@example.net\r\nSubject: local proxy\r\n\r\noriginal\r\n")
	session := &smtpSession{}
	checkError(t, session.Mail("sender@example.net", nil))
	checkError(t, session.Rcpt("forward@t12e.cc", nil))
	checkError(t, session.Rcpt("root@t12e.cc", nil))
	checkError(t, session.Data(bytes.NewReader(body)))
	mailbox, err := messages("t12e.cc/root")
	checkError(t, err)
	if len(mailbox) != 1 || !bytes.Equal(mailbox[0].Body, body) {
		t.Fatal("local proxy lost or duplicated mail")
	}
	mailbox, err = messages("t12e.cc/forward")
	checkError(t, err)
	if len(mailbox) != 0 {
		t.Fatal("proxy kept a local copy")
	}
	for _, target := range []string{"forward@t12e.cc", "bad", "one@example.net,two@example.net", "name <one@example.net>", "../root@t12e.cc", "missing@t12e.cc", "remote@example.net"} {
		checkError(t, objects.Put("t12e.cc/forward/.proxy", []byte(target)))
		if (&smtpSession{}).Rcpt("forward@t12e.cc", nil) == nil {
			t.Fatal("invalid/disabled proxy accepted", target)
		}
	}
	checkError(t, objects.Put("t12e.cc/forward/.proxy", []byte("alias@t12e.cc")))
	checkError(t, objects.Put("t12e.cc/alias/.alias", []byte("forward")))
	if (&smtpSession{}).Rcpt("forward@t12e.cc", nil) == nil {
		t.Fatal("mixed alias/proxy loop accepted")
	}
}

func TestProxyRemoteDurabilityAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			outboundTestDir(t)
			relay := newFakeRelay(t, true)
			if failure {
				relay.code.Store(550)
			}
			for _, id := range []string{"t12e.cc/first", "t12e.cc/second"} {
				checkError(t, objects.Put(id+"/.kind", []byte("proxy")))
				checkError(t, objects.Put(id+"/.proxy", []byte("destination@example.net")))
			}
			body := []byte("From: sender@example.net\r\nTo: first@t12e.cc\r\nSubject: forwarded\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAwQF\r\n")
			s := &smtpSession{}
			checkError(t, s.Mail("sender@example.net", nil))
			checkError(t, s.Rcpt("first@t12e.cc", nil))
			checkError(t, s.Rcpt("second@t12e.cc", nil))
			// Retargeting after RCPT must not alter the accepted routing snapshot.
			checkError(t, objects.Put("t12e.cc/first/.proxy", []byte("changed@example.net")))
			checkError(t, s.Data(bytes.NewReader(body)))
			key, job := queuedJob(t)
			if job.User != "" || job.From != "sender@example.net" || len(job.Recipients) != 1 || job.Recipients[0].Address != "destination@example.net" || len(job.Recipients[0].ProxyOwners) != 2 {
				t.Fatal("proxy routing was not persisted")
			}
			if !bytes.Equal(job.Body, append([]byte("X-FMA-Proxy-Hops: 1\r\n"), body...)) {
				t.Fatal("forwarding changed MIME content")
			}
			for _, id := range []string{"t12e.cc/first", "t12e.cc/second"} {
				mailbox, err := messages(id)
				checkError(t, err)
				if len(mailbox) != 0 {
					t.Fatal("proxy stored original mail")
				}
			}
			claimAndProcess(t, key) // Reloads the S3 task as a fresh worker.
			if _, err := objects.Get(key); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("completed proxy task/preclaim not deleted", err)
			}
			if !failure && relay.received.Load() != 1 {
				t.Fatal("duplicate proxy delivery")
			}
			if failure {
				for _, id := range []string{"t12e.cc/first", "t12e.cc/second"} {
					report, err := objects.Get(id + "/.proxy-errors/" + job.ID + ".json")
					checkError(t, err)
					if !bytes.Contains(report, []byte("destination@example.net")) || bytes.Contains(report, []byte("AAECAwQF")) {
						t.Fatal("invalid proxy failure report")
					}
				}
				if _, err := objects.Get("next"); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("failure wrote into empty account")
				}
			}
			s.Reset()
			if len(s.proxies) != 0 {
				t.Fatal("proxy route leaked into next SMTP transaction")
			}
		})
	}
}

func TestProxyHopLimit(t *testing.T) {
	body := []byte("Subject: loop\r\n\r\nbody\r\n")
	for range 16 {
		var err error
		body, err = proxyBody(body)
		checkError(t, err)
	}
	if _, err := proxyBody(body); err == nil {
		t.Fatal("external proxy loop not bounded")
	}
}

func TestJMAPS3BackendContract(t *testing.T) {
	outboundTestDir(t)
	backendtest.Run(t, backendtest.Config{
		Open: func(t *testing.T) jbackend.Backend {
			return &jmapBackend{key: t.Name() + "/.jmap/state.json", store: objects}
		},
		Reopen: func(t *testing.T, old jbackend.Backend) jbackend.Backend {
			b := old.(*jmapBackend)
			checkError(t, b.Close())
			jmapOwnerStates.Lock()
			delete(jmapOwnerStates.states, jmapOwnerCacheKey{b.store, b.key})
			jmapOwnerStates.Unlock()
			return &jmapBackend{key: b.key, store: b.store}
		},
	})
}

func TestJMAPSharedMailbox(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	body := []byte("From: alice@t12e.cc\r\nTo: alice@t12e.cc\r\nSubject: shared\r\nMessage-ID: <shared@t12e.cc>\r\n\r\nhello\r\n")
	checkError(t, appendMessage("t12e.cc/alice", body, nil, time.Now(), false))
	old := useJMAP
	useJMAP = true
	t.Cleanup(func() { useJMAP = old })
	a, err := openJMAPAccount(context.Background(), "t12e.cc/alice")
	checkError(t, err)
	result, err := a.call(context.Background(), "Email/query", map[string]any{})
	checkError(t, err)
	ids := jvalue[[]jmap.Id](result, "ids")
	if len(ids) != 1 {
		t.Fatalf("bootstrap ids: %s", result)
	}
	stored, err := messages("t12e.cc/alice")
	checkError(t, err)
	if len(stored) != 1 || stored[0].Uid != 1 || stored[0].Body != nil {
		t.Fatalf("shared mailbox: %+v", stored)
	}
	refs, err := mailBlobRefs(context.Background(), "t12e.cc/alice")
	checkError(t, err)
	r, _, err := openStoredMessage(context.Background(), stored[0], refs)
	checkError(t, err)
	raw, err := io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if !bytes.Equal(raw, body) {
		t.Fatal("streamed mailbox body differs")
	}
	_, err = a.call(context.Background(), "Email/set", map[string]any{"update": map[jmap.Id]any{ids[0]: map[string]any{"keywords/$seen": true}}})
	checkError(t, err)
	stored, err = messages("t12e.cc/alice")
	checkError(t, err)
	if !slices.Contains(stored[0].Flags, imap.SeenFlag) {
		t.Fatal("JMAP flags not visible in IMAP")
	}
	checkError(t, appendMessage("t12e.cc/alice", body, nil, time.Now(), false))
	stored, err = messages("t12e.cc/alice")
	checkError(t, err)
	if len(stored) != 2 || stored[1].Uid != 2 {
		t.Fatalf("append: %+v", stored)
	}
	checkError(t, removeMessage("t12e.cc/alice", 1))
	result, err = a.call(context.Background(), "Email/query", map[string]any{})
	checkError(t, err)
	if len(jvalue[[]jmap.Id](result, "ids")) != 1 {
		t.Fatal("POP/IMAP deletion not visible in JMAP")
	}
}

// Boundary checks use the actual S3 object's metadata and gzip bytes, then read
// through fma's adapter and verify the original size and hash.
func TestS3BlobCompression(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires native S3 fixture")
	}
	bucket, err := connectBucket(S3Config{Endpoint: endpoint, Bucket: os.Getenv("TEST_S3_BUCKET"), Region: "us-east-1", AccessKey: "test", SecretKey: "test"})
	checkError(t, err)
	root := fmt.Sprintf("t12e.cc/compression%d", time.Now().UnixNano())
	blobs := jmapBlobs{store: bucket}
	acct := jmapAccountID(root)
	t.Cleanup(func() {
		keys, _ := bucket.List(root + "/")
		for _, key := range keys {
			bucket.Delete(key)
		}
	})
	for _, tc := range []struct {
		size   int64
		random bool
	}{{gzipThreshold - 1, false}, {gzipThreshold, false}, {gzipThreshold + 1, false}, {17 << 20, false}, {17 << 20, true}} {
		size := tc.size
		t.Run(fmt.Sprintf("%d-random-%v", size, tc.random), func(t *testing.T) {
			ctx := context.Background()
			w, err := blobs.Create(ctx, acct)
			checkError(t, err)
			defer w.Abort()
			hash := sha256.New()
			block := bytes.Repeat([]byte("binary\x00\xff"), 4096)
			if tc.random {
				block = make([]byte, 1<<20)
				_, err = cryptorand.Read(block)
				checkError(t, err)
			}
			for left := size; left > 0; {
				part := block[:min(left, int64(len(block)))]
				_, err = w.Write(part)
				checkError(t, err)
				hash.Write(part)
				left -= int64(len(part))
			}
			id, err := w.Commit()
			checkError(t, err)
			key, err := blobs.key(acct, id)
			checkError(t, err)
			stored, err := bucket.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket.bucket, Key: &key})
			checkError(t, err)
			if stored.Metadata["fma-reference"] != "1" {
				t.Fatal("expected a small named-object index")
			}
			referenceData, err := io.ReadAll(io.LimitReader(stored.Body, 8193))
			stored.Body.Close()
			checkError(t, err)
			ref, err := decodeObjectReference(key, referenceData)
			checkError(t, err)
			if !strings.HasPrefix(ref.Key, root+"/mail/untitled_") || !strings.HasSuffix(ref.Key, "/message.eml") {
				t.Fatalf("unexpected physical key %q", ref.Key)
			}
			stored, err = bucket.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket.bucket, Key: &ref.Key})
			checkError(t, err)
			stored.Metadata = ref.Metadata
			compressed := stored.Metadata["fma-encoding"] == "gzip"
			if compressed != (size > gzipThreshold) {
				t.Fatalf("compression=%v size=%d", compressed, size)
			}
			if compressed {
				prefix := make([]byte, 2)
				_, err = io.ReadFull(stored.Body, prefix)
				checkError(t, err)
				if !bytes.Equal(prefix, []byte{0x1f, 0x8b}) {
					t.Fatalf("not gzip: %x", prefix)
				}
				if !tc.random && *stored.ContentLength >= size {
					t.Fatal("compressible input was not reduced")
				}
			}
			stored.Body.Close()
			r, length, err := blobs.Open(ctx, acct, id)
			checkError(t, err)
			defer r.Close()
			got := sha256.New()
			n, err := io.Copy(got, r)
			checkError(t, err)
			if length != size || n != size || !bytes.Equal(hash.Sum(nil), got.Sum(nil)) {
				t.Fatalf("round trip size=%d/%d want=%d", length, n, size)
			}
			// Copies must preserve compression metadata.
			copyKey := root + "/copy"
			checkError(t, bucket.CopyStream(ctx, key, copyKey))
			r2, length, err := bucket.OpenStream(ctx, copyKey)
			checkError(t, err)
			got.Reset()
			n, err = io.Copy(got, r2)
			r2.Close()
			checkError(t, err)
			if length != size || n != size || !bytes.Equal(hash.Sum(nil), got.Sum(nil)) {
				t.Fatal("copy lost compression metadata")
			}
		})
	}
}

// Isolate compression + hashing from S3 and network latency.
func BenchmarkBlobUploadPipeline(b *testing.B) {
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i)
	}
	b.SetBytes(2 << 30)
	b.ReportAllocs()
	for b.Loop() {
		w := &jmapBlobWriter{ctx: context.Background(), account: jmapAccountID("t12e.cc/bench"), digest: sha256.New(), writer: discardObjectUpload{}}
		for range 2048 {
			if _, err := w.Write(block); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := w.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

type discardObjectUpload struct{}

func (discardObjectUpload) Write(p []byte) (int, error)            { return len(p), nil }
func (discardObjectUpload) Commit(string, map[string]string) error { return nil }
func (discardObjectUpload) Abort() error                           { return nil }

func TestBufferPoolWaitCancelAndReuse(t *testing.T) {
	pool := newBufferPool(1, 128)
	first, err := pool.Get(context.Background())
	checkError(t, err)
	first.WriteString("private data")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := pool.Get(ctx); done <- err }()
	select {
	case <-done:
		t.Fatal("pool exceeded capacity")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled wait blocked")
	}
	pool.Put(first)
	next, err := pool.Get(context.Background())
	checkError(t, err)
	if next.Len() != 0 || next.Cap() != 128 {
		t.Fatal("buffer not reset")
	}
	pool.Put(next)
	if pool.outstanding() != 0 {
		t.Fatal("leaked permit")
	}
}

type shortErrorReader struct {
	left int
	err  error
}

func (r *shortErrorReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, r.err
	}
	n := min(7, min(len(p), r.left))
	clear(p[:n])
	r.left -= n
	return n, nil
}
func TestCopyStreamShortReadsAndErrors(t *testing.T) {
	for _, terminal := range []error{io.EOF, io.ErrUnexpectedEOF} {
		var dst bytes.Buffer
		n, err := copyStream(context.Background(), &dst, &shortErrorReader{left: 12345, err: terminal})
		if n != 12345 || dst.Len() != 12345 {
			t.Fatalf("short read lost bytes: %d", n)
		}
		if terminal == io.EOF && err != nil || terminal != io.EOF && !errors.Is(err, terminal) {
			t.Fatal(err)
		}
	}
	if copyBufferPool.outstanding() != 0 {
		t.Fatal("copy error leaked buffer")
	}
}
func TestBlobWriterAbortReturnsBuffers(t *testing.T) {
	for _, size := range []int{1, gzipThreshold, gzipThreshold + 1} {
		w := &jmapBlobWriter{ctx: context.Background(), account: jmapAccountID("t12e.cc/test"), digest: sha256.New(), writer: discardObjectUpload{}}
		block := make([]byte, 32768)
		for left := size; left > 0; {
			n := min(left, len(block))
			_, err := w.Write(block[:n])
			checkError(t, err)
			left -= n
		}
		checkError(t, w.Abort())
		checkError(t, w.Abort())
		if mailProbePool.outstanding() != 0 {
			t.Fatal("abort leaked probe buffer")
		}
	}
}
func TestGzipObjectReaderCorruption(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write(bytes.Repeat([]byte("payload"), 10000))
	checkError(t, err)
	checkError(t, writer.Close())
	valid := bytes.Clone(compressed.Bytes())
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 1
	for _, data := range [][]byte{valid, valid[:len(valid)-4], corrupt} {
		source := &closeTrackingReader{Reader: bytes.NewReader(data)}
		reader, err := gzip.NewReader(source)
		checkError(t, err)
		stream := &gzipObjectReader{Reader: reader, source: source}
		n, readErr := copyStream(context.Background(), io.Discard, stream)
		checkError(t, stream.Close())
		if !source.closed {
			t.Fatal("S3 reader was not closed")
		}
		if bytes.Equal(data, valid) {
			if n != 70000 || readErr != nil {
				t.Fatalf("round trip: %d %v", n, readErr)
			}
		} else if readErr == nil {
			t.Fatal("corrupt gzip accepted")
		}
	}
}

type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error { r.closed = true; return nil }

func TestWaitPoolConcurrentCapacity(t *testing.T) {
	pool := newBufferPool(3, 256)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				buffer, err := pool.Get(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				if n > 3 {
					t.Error("capacity exceeded")
				}
				buffer.Write(bytes.Repeat([]byte{byte(worker)}, 256))
				for _, value := range buffer.Bytes() {
					if value != byte(worker) {
						t.Error("buffer shared by concurrent callers")
						break
					}
				}
				active.Add(-1)
				pool.Put(buffer)
			}
		}()
	}
	wg.Wait()
	if pool.outstanding() != 0 || active.Load() != 0 {
		t.Fatal("leaked borrower")
	}
	if peak.Load() > 3 {
		t.Fatal("maximum exceeded")
	}
}

func TestNamedBlobKeyEscapesSegments(t *testing.T) {
	name := blobName{MIME: true, Filename: "../report/#?%.pdf"}
	key := namedBlobKey("t12e.cc/alice", name, []byte("Subject: =?UTF-8?B?5oql5Lu3L+WQiOWQjCAjMQ==?=\r\n\r\nbody"))
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[0] != "t12e.cc" || parts[1] != "alice" || parts[2] != "mail" {
		t.Fatalf("unsafe key %q", key)
	}
	if !strings.HasPrefix(parts[3], "%E6%8A%A5%E4%BB%B7%2F%E5%90%88%E5%90%8C%20%231_") {
		t.Fatalf("subject not decoded and escaped: %q", parts[3])
	}
	if parts[4] != "..%2Freport%2F%23%3F%25.pdf" {
		t.Fatalf("filename not escaped: %q", parts[4])
	}
	for _, value := range []string{".", "..", "a/b", "a%2Fb", "a?b#c", "\x00\r\n"} {
		segment := objectNameSegment(value, "untitled")
		if segment == "." || segment == ".." || strings.ContainsAny(segment, "/?#\x00\r\n") {
			t.Fatalf("unsafe segment %q", segment)
		}
	}
	if len(namedBlobKey("t12e.cc/alice", blobName{Subject: strings.Repeat("界", 1000), Filename: strings.Repeat("文", 1000)}, nil)) > 1024 {
		t.Fatal("S3 key limit exceeded")
	}
}

func TestObjectReferenceAccountBoundary(t *testing.T) {
	for _, target := range []string{"t12e.cc/bob/mail/topic/file", "t12e.cc/alice/mail/../../bob/file", "t12e.cc/alice/.jmap/blobs/Gother", "/alice/mail/topic/file"} {
		data := fmt.Appendf(nil, `{"key":%q}`, target)
		if _, err := decodeObjectReference("t12e.cc/alice/.jmap/blobs/Gid", data); err == nil {
			t.Fatalf("accepted %q", target)
		}
	}
	if _, err := decodeObjectReference("t12e.cc/alice/.jmap/blobs/Gid", []byte(`{"key":"t12e.cc/alice/mail/topic/file","metadata":{"fma-reference":"1"}}`)); err == nil {
		t.Fatal("accepted nested reference")
	}
}

func TestSeparateUploadAndMIMELimits(t *testing.T) {
	if jmapCore().MaxSizeUpload != 4<<30 || jmapMailCapability().MaxSizeAttachmentsPerEmail != 4<<30 || maxMailSize != 6<<30 {
		t.Fatal("upload, attachment and MIME limits differ")
	}
	w := &jmapBlobWriter{ctx: context.Background(), account: jmapAccountID("t12e.cc/alice"), writer: discardObjectUpload{}, digest: sha256.New(), size: maxMailSize}
	if _, err := w.Write([]byte{1}); err == nil {
		t.Fatal("oversized MIME accepted")
	}
}

func TestS3NamedObjectCollisionAndLegacyRead(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires native S3 fixture")
	}
	bucket, err := connectBucket(S3Config{Endpoint: endpoint, Bucket: os.Getenv("TEST_S3_BUCKET"), Region: "us-east-1", AccessKey: "test", SecretKey: "test"})
	checkError(t, err)
	root := fmt.Sprintf("t12e.cc/names%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, account := range []string{root, root + "b"} {
			keys, _ := bucket.List(account + "/")
			for _, key := range keys {
				bucket.Delete(key)
			}
		}
	})
	ctx := context.Background()
	key := namedBlobKey(root, blobName{Subject: "a/b#%", Filename: "../report?.pdf"}, nil)
	first, err := bucket.NewUpload(ctx, key)
	checkError(t, err)
	_, err = first.Write([]byte("original"))
	checkError(t, err)
	index := root + "/.jmap/blobs/G" + strings.Repeat("a", 43)
	checkError(t, first.Commit(index, nil))
	checkError(t, first.Abort())
	if _, err = bucket.NewUpload(ctx, key); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("collision: %v", err)
	}
	multipartKey := namedBlobKey(root, blobName{Subject: "multipart", Filename: "large.bin"}, nil)
	multipartWriter, err := bucket.NewUpload(ctx, multipartKey)
	checkError(t, err)
	block := make([]byte, 128<<10)
	for range 136 {
		_, err = multipartWriter.Write(block)
		checkError(t, err)
	}
	if _, err = bucket.NewUpload(ctx, multipartKey); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("multipart collision: %v", err)
	}
	checkError(t, multipartWriter.Commit(root+"/.jmap/blobs/multipart", nil))
	checkError(t, multipartWriter.Abort())
	abortedKey := namedBlobKey(root, blobName{Subject: "aborted"}, nil)
	aborted, err := bucket.NewUpload(ctx, abortedKey)
	checkError(t, err)
	checkError(t, aborted.Abort())
	retried, err := bucket.NewUpload(ctx, abortedKey)
	checkError(t, err)
	checkError(t, retried.Abort())
	r, n, err := bucket.OpenStream(ctx, index)
	checkError(t, err)
	data, err := io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if n != 8 || string(data) != "original" {
		t.Fatal("collision replaced original data")
	}
	destination := root + "t12e.cc/b/.jmap/blobs/" + path.Base(index)
	checkError(t, bucket.CopyStream(ctx, index, destination))
	checkError(t, bucket.CopyStream(ctx, index, destination))
	r, n, err = bucket.OpenStream(ctx, destination)
	checkError(t, err)
	data, err = io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if n != 8 || string(data) != "original" {
		t.Fatal("cross-account copy lost content")
	}
	empty, err := bucket.NewUpload(ctx, namedBlobKey(root, blobName{Filename: "empty.bin"}, nil))
	checkError(t, err)
	emptyIndex := root + "/.jmap/blobs/G" + strings.Repeat("b", 43)
	checkError(t, empty.Commit(emptyIndex, nil))
	checkError(t, empty.Abort())
	emptyDestination := root + "t12e.cc/b/.jmap/blobs/" + path.Base(emptyIndex)
	checkError(t, bucket.CopyStream(ctx, emptyIndex, emptyDestination))
	r, n, err = bucket.OpenStream(ctx, emptyDestination)
	checkError(t, err)
	data, err = io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if n != 0 || len(data) != 0 {
		t.Fatal("empty copy contains data")
	}
	legacy := root + "/.jmap/blobs/legacy"
	checkError(t, bucket.Put(legacy, []byte("old blob")))
	r, n, err = bucket.OpenStream(ctx, legacy)
	checkError(t, err)
	data, err = io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if n != 8 || string(data) != "old blob" {
		t.Fatal("legacy blob no longer readable")
	}
}

// Exercise the actual HTTP copy pattern, including malformed MIME and readers
// that return data together with an error. A bounded number of writes is the
// regression check for the short-read bottleneck.
type jmapReadFunc func([]byte) (int, error)

func (f jmapReadFunc) Read(p []byte) (int, error) { return f(p) }

type jmapCountingWriter struct {
	bytes.Buffer
	writes int
}

func (w *jmapCountingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestJMAPDownloadReader(t *testing.T) {
	t.Run("base64 HTTP writes", func(t *testing.T) {
		want := bytes.Repeat([]byte("attachment\x00\xff"), 100000)
		encoded := base64.StdEncoding.EncodeToString(want)
		reader := &jmapDownloadReader{ctx: context.Background(), reader: base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))}
		writer := new(jmapCountingWriter)
		// The destination also exposes ReadFrom. WriteTo must keep writes
		// on the bounded copy path instead of dispatching into it.
		n, err := io.Copy(writer, reader)
		checkError(t, err)
		if n != int64(len(want)) || !bytes.Equal(writer.Bytes(), want) {
			t.Fatal("decoded content changed")
		}
		if writer.writes != (len(want)+(128<<10)-1)/(128<<10) {
			t.Fatalf("short reads escaped to HTTP: %d writes", writer.writes)
		}
	})
	t.Run("errors retain partial bytes", func(t *testing.T) {
		for _, terminal := range []error{io.EOF, io.ErrUnexpectedEOF, base64.CorruptInputError(3)} {
			source := jmapReadFunc(func(p []byte) (int, error) { return copy(p, "abc"), terminal })
			reader := &jmapDownloadReader{ctx: context.Background(), reader: source}
			p := make([]byte, 20)
			n, err := reader.Read(p)
			if n != 3 || string(p[:n]) != "abc" || err != terminal {
				t.Fatalf("%d %q %v", n, p[:n], err)
			}
			n, err = reader.Read(p)
			if n != 0 || err != terminal {
				t.Fatalf("terminal error lost: %d %v", n, err)
			}
			n, err = reader.Read(nil)
			if n != 0 || err != nil {
				t.Fatalf("empty read: %d %v", n, err)
			}
		}
	})
	t.Run("cancellation and close", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := &closeTrackingReader{Reader: jmapReadFunc(func(p []byte) (int, error) {
			cancel()
			return copy(p, "abc"), nil
		})}
		reader := &jmapDownloadReader{ctx: ctx, reader: source, Closer: source}
		p := make([]byte, 20)
		n, err := reader.Read(p)
		if n != 3 || string(p[:n]) != "abc" || !errors.Is(err, context.Canceled) {
			t.Fatalf("%d %q %v", n, p[:n], err)
		}
		checkError(t, reader.Close())
		if !source.closed {
			t.Fatal("source not closed")
		}
	})
	t.Run("no progress", func(t *testing.T) {
		reader := &jmapDownloadReader{ctx: context.Background(), reader: jmapReadFunc(func([]byte) (int, error) { return 0, nil })}
		n, err := reader.Read(make([]byte, 20))
		if n != 0 || err != io.ErrNoProgress {
			t.Fatalf("%d %v", n, err)
		}
	})
	t.Run("bounded read ahead", func(t *testing.T) {
		source := &shortErrorReader{left: 1 << 20, err: io.EOF}
		reader := &jmapDownloadReader{ctx: context.Background(), reader: source}
		n, err := reader.Read(make([]byte, 1<<20))
		checkError(t, err)
		if n != 128<<10 || source.left != (1<<20)-(128<<10) {
			t.Fatalf("read ahead: %d", n)
		}
	})
}

type jmapWriteFunc func([]byte) (int, error)

func (f jmapWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestJMAPDownloadWriterFailure(t *testing.T) {
	for _, terminal := range []error{nil, io.ErrClosedPipe} {
		reader := &jmapDownloadReader{ctx: context.Background(), reader: strings.NewReader(strings.Repeat("x", 1<<20))}
		before := copyBufferPool.outstanding()
		n, err := reader.WriteTo(jmapWriteFunc(func(p []byte) (int, error) { return 13, terminal }))
		want := terminal
		if want == nil {
			want = io.ErrShortWrite
		}
		if n != 13 || err != want {
			t.Fatalf("partial write: %d %v", n, err)
		}
		if copyBufferPool.outstanding() != before {
			t.Fatal("writer failure leaked pooled buffer")
		}
	}
}

type mimeChunkReader struct {
	io.Reader
	limit int
}

func (r mimeChunkReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), r.limit)]) }

func TestMIMEBase64Reader(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 4095, 32767, 65537} {
		raw := bytes.Repeat([]byte("a\x00\xff"), (size+2)/3)[:size]
		encoded := base64.StdEncoding.EncodeToString(raw)
		var folded strings.Builder
		for len(encoded) > 0 {
			n := min(76, len(encoded))
			folded.WriteString(encoded[:n])
			folded.WriteString("\r\n")
			encoded = encoded[n:]
		}
		for _, input := range []int{1, 3, 77, 1024, 4096, 32768} {
			for _, output := range []int{1, 7, 128 << 10} {
				reader := &mimeBase64Reader{source: mimeChunkReader{strings.NewReader(folded.String()), input}}
				var got bytes.Buffer
				_, err := io.CopyBuffer(struct{ io.Writer }{&got}, reader, make([]byte, output))
				checkError(t, err)
				if !bytes.Equal(got.Bytes(), raw) {
					t.Fatalf("content mismatch size=%d input=%d output=%d", size, input, output)
				}
				if reader.buffer != nil {
					t.Fatal("EOF retained pooled buffer")
				}
			}
		}
	}
}

func TestMIMEBase64ReaderErrors(t *testing.T) {
	for _, raw := range []string{"Y", "YQ=", "YWJj#", "YWJj\t", "YQ==x"} {
		for _, chunk := range []int{1, 4, 1024, 32768} {
			reader := &mimeBase64Reader{source: mimeChunkReader{strings.NewReader(raw), chunk}}
			_, err := io.ReadAll(reader)
			if err == nil {
				t.Fatalf("accepted corrupt input %q chunk=%d", raw, chunk)
			}
			if reader.buffer != nil {
				t.Fatal("error retained pooled buffer")
			}
		}
	}
	for _, raw := range []string{"YQ==", "YQ==\r\n", "YWJj\r\n", "Y\rW\nJj"} {
		got, err := io.ReadAll(&mimeBase64Reader{source: strings.NewReader(raw)})
		checkError(t, err)
		want, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(raw)))
		checkError(t, err)
		if !bytes.Equal(got, want) {
			t.Fatalf("newline handling: %q", raw)
		}
	}
	terminal := errors.New("source interrupted")
	reader := &mimeBase64Reader{source: jmapReadFunc(func(p []byte) (int, error) { return copy(p, "YWJj"), terminal })}
	got, err := io.ReadAll(reader)
	if string(got) != "abc" || err != terminal || reader.buffer != nil {
		t.Fatalf("source error: %q %v", got, err)
	}
	reader = &mimeBase64Reader{source: jmapReadFunc(func([]byte) (int, error) { return 0, nil })}
	_, err = io.ReadAll(reader)
	if err != io.ErrNoProgress || reader.buffer != nil {
		t.Fatalf("empty reader: %v", err)
	}
}

func BenchmarkMIMEBase64(b *testing.B) {
	raw := bytes.Repeat([]byte("stream\x00\xff"), (1<<20)/8)
	encoded := base64.StdEncoding.EncodeToString(raw)
	var folded strings.Builder
	for len(encoded) > 0 {
		n := min(76, len(encoded))
		folded.WriteString(encoded[:n])
		folded.WriteString("\r\n")
		encoded = encoded[n:]
	}
	data := folded.String()
	expected := sha256.Sum256(raw)
	for _, variant := range []string{"standard", "block"} {
		b.Run(variant, func(b *testing.B) {
			buf := make([]byte, 128<<10)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var reader io.Reader
				if variant == "block" {
					reader = &mimeBase64Reader{source: strings.NewReader(data)}
				} else {
					reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
				}
				digest := sha256.New()
				_, err := io.CopyBuffer(digest, reader, buf)
				if err != nil || !bytes.Equal(digest.Sum(nil), expected[:]) {
					b.Fatalf("decoded digest: %v", err)
				}
			}
		})
	}
}

func TestS3PartBufferBlocksAndRetry(t *testing.T) {
	p, err := s3BufferPool.Get(context.Background())
	checkError(t, err)
	defer s3BufferPool.Put(p)
	payload := bytes.Repeat([]byte("block\x00\xff"), (2*s3BlockSize)/7+1)
	n, err := p.Write(payload)
	checkError(t, err)
	if n != len(payload) || p.Len() != len(payload) {
		t.Fatal("partial block write")
	}
	for _, block := range p.blocks {
		if block != nil && (len(block) != s3BlockSize || cap(block) != s3BlockSize) {
			t.Fatal("block exceeds 1 MiB")
		}
	}
	for _, offset := range []int64{0, s3BlockSize - 3, s3BlockSize, 2*s3BlockSize - 1, int64(len(payload)), int64(len(payload) + 1)} {
		_, err = p.Seek(offset, io.SeekStart)
		checkError(t, err)
		got, err := io.ReadAll(p)
		checkError(t, err)
		if !bytes.Equal(got, payload[min(int(offset), len(payload)):]) {
			t.Fatalf("seek/read mismatch at %d", offset)
		}
	}
	_, err = p.Seek(-5, io.SeekEnd)
	checkError(t, err)
	got, err := io.ReadAll(p)
	checkError(t, err)
	if !bytes.Equal(got, payload[len(payload)-5:]) {
		t.Fatal("retry tail changed")
	}
	if _, err = p.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	if _, err = p.Seek(0, 99); err == nil {
		t.Fatal("invalid seek accepted")
	}
	if _, err = p.Write(make([]byte, s3PartSize)); err != io.ErrShortWrite {
		t.Fatal("part overflow accepted")
	}
	if s3BufferPool.max*s3PartSize != s3BufferLimit || s3BufferLimit != 1<<30 {
		t.Fatal("incorrect global buffer bound")
	}
}

func TestS3CopyStreamStaysOnServer(t *testing.T) {
	for _, physicalSize := range []int64{16 << 20, 6 << 30} {
		t.Run(fmt.Sprint(physicalSize), func(t *testing.T) {
			source := "t12e.cc/alice/.jmap/blobs/G" + strings.Repeat("a", 43)
			destination := "t12e.cc/bob/.jmap/blobs/G" + strings.Repeat("a", 43)
			physical := "t12e.cc/alice/mail/topic_123/message.eml"
			ref := namedObjectReference{Key: physical, Metadata: map[string]string{"fma-encoding": "gzip", "fma-size": "2147483648"}}
			data, err := json.Marshal(ref)
			checkError(t, err)
			var copies, bodyReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := strings.TrimPrefix(r.URL.Path, "/bucket/")
				switch {
				case r.Method == "HEAD" && r.URL.Path == "/bucket":
					w.WriteHeader(200)
				case r.Method == "HEAD" && key == source:
					w.Header().Set("X-Amz-Meta-Fma-Reference", "1")
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					w.WriteHeader(200)
				case r.Method == "GET" && key == source:
					w.Write(data)
				case r.Method == "HEAD" && key == destination:
					w.WriteHeader(404)
				case r.Method == "HEAD" && key == physical:
					w.Header().Set("Content-Length", fmt.Sprint(physicalSize))
					w.Header().Set("ETag", `"original"`)
					w.WriteHeader(200)
				case r.Method == "GET" && key == physical:
					bodyReads.Add(1)
					http.Error(w, "application must not read object body", 500)
				case r.Method == "POST" && r.URL.Query().Has("uploads"):
					if r.Header.Get("X-Amz-Meta-Fma-Encoding") != "gzip" {
						t.Error("multipart copy lost metadata")
					}
					fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>copy-upload</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == "POST" && r.URL.Query().Get("uploadId") == "copy-upload":
					fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"completed"</ETag></CompleteMultipartUploadResult>`)
				case r.Method == "PUT" && r.Header.Get("X-Amz-Copy-Source") != "":
					copies.Add(1)
					if physicalSize <= 5<<30 && (r.Header.Get("X-Amz-Meta-Fma-Encoding") != "gzip" || r.Header.Get("X-Amz-Meta-Fma-Size") != "2147483648") {
						t.Error("copy lost compression metadata")
					}
					w.Header().Set("Content-Type", "application/xml")
					if r.URL.Query().Get("uploadId") == "copy-upload" {
						if r.Header.Get("X-Amz-Copy-Source-Range") == "" {
							t.Error("multipart copy missing range")
						}
						fmt.Fprint(w, `<CopyPartResult><ETag>"copied-part"</ETag></CopyPartResult>`)
					} else {
						fmt.Fprint(w, `<CopyObjectResult><ETag>"copied"</ETag></CopyObjectResult>`)
					}
				case r.Method == "PUT":
					if key == destination {
						var copied namedObjectReference
						if err := json.NewDecoder(r.Body).Decode(&copied); err != nil || copied.Key != "t12e.cc/bob/mail/topic_123/message.eml" || copied.Metadata["fma-encoding"] != "gzip" {
							t.Error("invalid destination reference", err)
						}
					}
					w.Header().Set("ETag", `"reserved"`)
					w.WriteHeader(200)
				default:
					http.Error(w, "unexpected S3 operation", 500)
				}
			}))
			defer server.Close()
			bucket, err := connectBucket(S3Config{Endpoint: server.URL, Bucket: "bucket", Region: "us-east-1", AccessKey: "test", SecretKey: "test"})
			checkError(t, err)
			checkError(t, bucket.CopyStream(context.Background(), source, destination))
			expectedCopies := int32(1)
			if physicalSize > 5<<30 {
				expectedCopies = int32((physicalSize + (512 << 20) - 1) / (512 << 20))
			}
			if copies.Load() != expectedCopies || bodyReads.Load() != 0 {
				t.Fatalf("copies=%d body reads=%d", copies.Load(), bodyReads.Load())
			}
		})
	}
}

// Deny reads of the complete MIME after import. Metadata, first attachment
// authorization and download must work even after reopening the account.
type denyMIMEStore struct {
	objectStore
	denied string
	reads  int
}

func (s *denyMIMEStore) GetVersion(key string) ([]byte, string, error) {
	return s.objectStore.(versionedStore).GetVersion(key)
}
func (s *denyMIMEStore) Swap(key string, data []byte, version string) error {
	return s.objectStore.(versionedStore).Swap(key, data, version)
}
func (s *denyMIMEStore) Get(key string) ([]byte, error) {
	if key == s.denied {
		s.reads++
		return nil, fmt.Errorf("unexpected complete MIME read")
	}
	return s.objectStore.Get(key)
}
func TestJMAPStoredPartsFirstRead(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	old := useJMAP
	useJMAP = true
	t.Cleanup(func() { useJMAP = old })
	ctx := context.Background()
	a, err := openJMAPAccount(ctx, "t12e.cc/alice")
	checkError(t, err)
	box, err := a.mailbox(ctx, "t12e.cc/alice")
	checkError(t, err)
	raw := []byte("From: a@example.com\r\nSubject: stored / ?\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nhello\r\n--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"../same.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAwQ=\r\n--x--\r\n")
	id, err := a.importMail(ctx, box, raw, nil, time.Now())
	checkError(t, err)
	email, err := a.db.Get(ctx, a.id, "Email", id)
	checkError(t, err)
	source := jvalue[jmap.Id](email, "blobId")
	sourceKey, err := a.blobs.key(a.id, source)
	checkError(t, err)
	store := &denyMIMEStore{objectStore: a.blobs.store, denied: sourceKey}
	reopened, err := newJMAPAccount("t12e.cc/alice", store)
	checkError(t, err)
	result, err := reopened.call(ctx, "Email/get", map[string]any{"ids": []jmap.Id{id}, "properties": []string{"id", "attachments"}})
	checkError(t, err)
	list := jvalue[[]map[string]json.RawMessage](result, "list")
	var parts []struct {
		BlobID jmap.Id `json:"blobId"`
		Size   int64   `json:"size"`
	}
	checkError(t, json.Unmarshal(list[0]["attachments"], &parts))
	if len(parts) != 1 || parts[0].Size != 5 {
		t.Fatalf("parts: %+v", parts)
	}
	checkError(t, reopened.materializePart(ctx, parts[0].BlobID, "t12e.cc/alice"))
	r, n, err := reopened.blobs.Open(ctx, reopened.id, parts[0].BlobID)
	checkError(t, err)
	got, err := io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if n != 5 || !bytes.Equal(got, []byte{0, 1, 2, 3, 4}) || store.reads != 0 {
		t.Fatalf("first read: %x, MIME reads=%d", got, store.reads)
	}
	bob, err := newJMAPAccount("t12e.cc/bob", store)
	checkError(t, err)
	if err = bob.materializePart(ctx, parts[0].BlobID, "t12e.cc/bob"); err == nil {
		t.Fatal("cross-account attachment access")
	}
}

func TestMIMEStreamBlocksOwnershipAndCancellation(t *testing.T) {
	data := make([]byte, 5<<20)
	for i := range data {
		data[i] = byte(i*17 + i/251)
	}
	blocks := newMIMEStreamBlocks()
	done := make(chan error, 1)
	var raw bytes.Buffer
	go func() { _, err := blocks.receive(context.Background(), bytes.NewReader(data), &raw); done <- err }()
	var decoded bytes.Buffer
	buf := make([]byte, 7777)
	_, err := io.CopyBuffer(struct{ io.Writer }{&decoded}, blocks, buf)
	checkError(t, err)
	checkError(t, <-done)
	if !bytes.Equal(raw.Bytes(), data) || !bytes.Equal(decoded.Bytes(), data) {
		t.Fatal("reused block changed unread data")
	}
	for range 3 {
		if b := <-blocks.free; len(b) != 1<<20 || cap(b) != 1<<20 {
			t.Fatal("lost original block allocation")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := newMIMEStreamBlocks()
	go func() { _, err := blocked.receive(ctx, bytes.NewReader(data), io.Discard); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer stuck after cancellation")
	}
	if mailProbePool == partProbePool || gzipWriters == partGzipWriters {
		t.Fatal("dependent streams share an exhaustible pool")
	}
}

type mimeConsumerFunc func([]byte) (int, error)

func (f mimeConsumerFunc) Write(b []byte) (int, error) { return f(b) }

func TestMIMEStreamParallelConsumers(t *testing.T) {
	data := bytes.Repeat([]byte("ordered immutable input"), 200000)
	blocks := newMIMEStreamBlocks()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var outputs [2]bytes.Buffer
	writers := make([]io.Writer, 2)
	for i := range writers {
		first := true
		writers[i] = mimeConsumerFunc(func(b []byte) (int, error) {
			if first {
				first = false
				entered <- struct{}{}
				<-release
			}
			return outputs[i].Write(b)
		})
	}
	received := make(chan error, 1)
	parsed := make(chan error, 1)
	go func() {
		_, err := blocks.receive(context.Background(), bytes.NewReader(data), writers...)
		received <- err
	}()
	var result bytes.Buffer
	go func() { _, err := io.Copy(&result, blocks); parsed <- err }()
	// Both writers must enter before either is allowed to complete its first write.
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("writers are serialized")
		}
	}
	close(release)
	checkError(t, <-received)
	checkError(t, <-parsed)
	for _, got := range [][]byte{result.Bytes(), outputs[0].Bytes(), outputs[1].Bytes()} {
		if !bytes.Equal(got, data) {
			t.Fatal("parallel consumer saw reordered or reused data")
		}
	}
	for range 3 {
		<-blocks.free
	}
}

func TestMIMEStreamWriterFailure(t *testing.T) {
	failure := errors.New("storage write failed")
	for _, want := range []error{failure, io.ErrShortWrite} {
		t.Run(want.Error(), func(t *testing.T) {
			blocks := newMIMEStreamBlocks()
			done := make(chan error, 1)
			writer := mimeConsumerFunc(func(b []byte) (int, error) {
				if want == io.ErrShortWrite {
					return len(b) - 1, nil
				}
				return 0, failure
			})
			go func() {
				_, err := blocks.receive(context.Background(), bytes.NewReader(make([]byte, 5<<20)), writer, io.Discard)
				done <- err
			}()
			// No parser drains ready: cancellation must wake the blocked producer anyway.
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed consumer stranded producer")
			}
		})
	}
}

func TestStreamWorkerConfiguration(t *testing.T) {
	c, err := loadConfig(nil, func(string) string { return "" })
	checkError(t, err)
	if c.StreamWorkers != 4 {
		t.Fatal("default worker count")
	}
	env := func(k string) string {
		if k == "FMA_STREAM_WORKERS" {
			return "3"
		}
		return ""
	}
	c, err = loadConfig(nil, env)
	checkError(t, err)
	if c.StreamWorkers != 3 {
		t.Fatal("worker environment ignored")
	}
	c, err = loadConfig([]string{"-stream-workers", "5"}, env)
	checkError(t, err)
	if c.StreamWorkers != 5 {
		t.Fatal("worker flag precedence")
	}
	for _, v := range []string{"0", "-1", "129", "abc"} {
		if _, err := loadConfig([]string{"-stream-workers", v}, env); err == nil {
			t.Fatal("invalid workers accepted", v)
		}
	}
}
func TestStreamTaskPoolDispatchAndShutdown(t *testing.T) {
	pool := newStreamTaskPool(8)
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var active, peak atomic.Int32
	var results []<-chan error
	for range 8 {
		done, err := pool.Submit(context.Background(), func(ctx context.Context) error {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		checkError(t, err)
		results = append(results, done)
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers failed to run concurrently")
		}
	}
	if peak.Load() != 8 {
		t.Fatal("not all workers dispatched")
	}
	var cancelledRuns atomic.Int32
	for range 8 {
		done, err := pool.Submit(context.Background(), func(context.Context) error { cancelledRuns.Add(1); return nil })
		checkError(t, err)
		results = append(results, done)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Submit(ctx, func(context.Context) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("full-queue cancellation", err)
	}
	stopped := make(chan struct{})
	go func() { pool.Close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
	for _, done := range results {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("shutdown lost task result", err)
		}
	}
	if cancelledRuns.Load() != 0 {
		t.Fatal("ran queued tasks during shutdown")
	}
	if _, err := pool.Submit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, net.ErrClosed) {
		t.Fatal("submission after shutdown", err)
	}
}

// A fresh store identity simulates another process without sharing any caches.
type ownerTestStore struct {
	objectStore
	versionedStore
	reads, writes, lists int
	failHead             bool
}

func (s *ownerTestStore) Get(key string) ([]byte, error) { s.reads++; return s.objectStore.Get(key) }
func (s *ownerTestStore) GetVersion(key string) ([]byte, string, error) {
	s.reads++
	return s.versionedStore.GetVersion(key)
}
func (s *ownerTestStore) Create(key string, value []byte) error {
	s.writes++
	return s.objectStore.Create(key, value)
}
func (s *ownerTestStore) Swap(key string, value []byte, version string) error {
	s.writes++
	if s.failHead && strings.HasSuffix(key, "/state.json") {
		return fs.ErrPermission
	}
	return s.versionedStore.Swap(key, value, version)
}
func (s *ownerTestStore) List(prefix string) ([]string, error) {
	s.lists++
	return s.objectStore.List(prefix)
}
func ownerTestSnapshot(t *testing.T, s *jmapOwnerState) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for key, value := range s.data {
		raw, err := hex.DecodeString(key)
		checkError(t, err)
		parts := jmapKeySegments(raw)
		// Pending garbage-collection hints are supersets, and tags are worklists.
		if len(parts) > 1 && (parts[1] == "x" || parts[1] == "r") {
			result[key] = bytes.Clone(value)
		}
	}
	return result
}
func TestJMAPOwnerColdRecoveryAndLayout(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("t12e.cc/alice/.kind", []byte("account")))
	checkError(t, objects.Put("t12e.cc/alice/.password", []byte("secret")))
	ctx := context.Background()
	a, err := openJMAPAccount(ctx, "t12e.cc/alice")
	checkError(t, err)
	box, err := a.mailbox(ctx, "t12e.cc/alice")
	checkError(t, err)
	body := []byte("From: sender@example.com\r\nSubject: owner storage\r\nMessage-ID: <owner@example.com>\r\n\r\nhello\r\n")
	id, err := a.importMail(ctx, box, body, nil, time.Now())
	checkError(t, err)
	backend := &jmapBackend{key: "t12e.cc/alice/.jmap/state.json", store: objects}
	state, err := backend.owner()
	checkError(t, err)
	expected := ownerTestSnapshot(t, state)
	if len(expected) == 0 {
		t.Fatal("missing live indexes")
	}
	raw, err := objects.Get(state.key)
	checkError(t, err)
	var head jmapOwnerFile
	checkError(t, json.Unmarshal(raw, &head))
	for key := range head.User {
		raw, err := hex.DecodeString(key)
		checkError(t, err)
		if jmapTransientKey(raw) || jmapOwnedKey(raw) {
			t.Fatal("account retained mail or indexes")
		}
	}
	keys, err := objects.List("t12e.cc/alice/")
	checkError(t, err)
	for _, key := range keys {
		if key == state.key || key == "t12e.cc/alice/.kind" || key == "t12e.cc/alice/.password" {
			continue
		}
		if !strings.HasPrefix(key, "t12e.cc/alice/mail/") {
			t.Fatalf("mail escaped prefix: %s", key)
		}
		if strings.HasSuffix(key, ".origin.json") {
			t.Fatal("persisted attachment reverse index")
		}
		if strings.Contains(key, ".fma.json") {
			data, err := objects.Get(key)
			checkError(t, err)
			var record jmapOwnerRecord
			checkError(t, json.Unmarshal(data, &record))
			raw, err := hex.DecodeString(record.Key)
			checkError(t, err)
			if jmapTransientKey(raw) {
				t.Fatal("persisted query index")
			}
		}
	}
	// Do not flush first: a killed process must recover the published decision.
	remote := &ownerTestStore{objectStore: objects, versionedStore: objects.(versionedStore)}
	cold, err := newJMAPAccount("t12e.cc/alice", remote)
	checkError(t, err)
	coldBackend := &jmapBackend{key: state.key, store: remote}
	recovered, err := coldBackend.owner()
	checkError(t, err)
	accounts, err := cold.db.Accounts(ctx)
	checkError(t, err)
	if !slices.Contains(accounts, cold.id) {
		t.Fatal("cold account discovery tag missing")
	}
	actual := ownerTestSnapshot(t, recovered)
	if len(expected) != len(actual) {
		t.Fatalf("index rows: live=%d cold=%d", len(expected), len(actual))
	}
	for key := range expected {
		if _, ok := actual[key]; !ok {
			t.Fatalf("lost index %x", key)
		}
	}
	email, err := cold.db.Get(ctx, cold.id, "Email", id)
	checkError(t, err)
	reader, _, err := cold.blobs.Open(ctx, cold.id, jvalue[jmap.Id](email, "blobId"))
	checkError(t, err)
	got, err := io.ReadAll(reader)
	reader.Close()
	checkError(t, err)
	if !bytes.Equal(got, body) {
		t.Fatal("cold blob content differs")
	}
	result, err := cold.call(ctx, "Email/query", map[string]any{"filter": map[string]any{"inMailbox": box}})
	checkError(t, err)
	if !slices.Contains(jvalue[[]jmap.Id](result, "ids"), id) {
		t.Fatal("cold membership query lost email")
	}
}

func TestJMAPOwnerAtomicFailureAndIncrementalWrites(t *testing.T) {
	base := &memoryObjects{data: map[string][]byte{}}
	store := &ownerTestStore{objectStore: base, versionedStore: base}
	ctx := context.Background()
	b := &jmapBackend{key: "t12e.cc/alice/.jmap/state.json", store: store}
	batch := &jbackend.Batch{}
	userKey := jmapIndexKey("Aalice", "q")
	mailKey := jmapIndexKey("Aalice", "o", "Thread", "T1")
	batch.Set(userKey, []byte("old"))
	batch.Set(mailKey, []byte(`{"id":"T1"}`))
	checkError(t, b.WriteBatch(ctx, batch))
	state, err := b.owner()
	checkError(t, err)
	checkError(t, state.flush(ctx))
	store.failHead = true
	failed := &jbackend.Batch{}
	failed.Set(userKey, []byte("new"))
	failed.Set(mailKey, []byte(`{"id":"T1","updated":true}`))
	if err := b.WriteBatch(ctx, failed); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("expected commit failure", err)
	}
	remote := &ownerTestStore{objectStore: base, versionedStore: base}
	cold := &jmapBackend{key: b.key, store: remote}
	got, err := cold.Get(ctx, userKey)
	checkError(t, err)
	if string(got) != "old" {
		t.Fatal("uncommitted user data visible")
	}
	got, err = cold.Get(ctx, mailKey)
	checkError(t, err)
	if string(got) != `{"id":"T1"}` {
		t.Fatal("uncommitted mail visible")
	}
	store.failHead = false
	checkError(t, b.WriteBatch(ctx, failed))
	// Cached reads do no S3 work, including MultiGet and bounded Scan.
	reads, lists := store.reads, store.lists
	for range 100 {
		_, err = b.MultiGet(ctx, [][]byte{mailKey, userKey})
		checkError(t, err)
		checkError(t, b.Scan(ctx, mailKey, nil, false, func(_, _ []byte) bool { return false }))
	}
	if store.reads != reads || store.lists != lists {
		t.Fatal("warm reads touched S3")
	}
	index := &jbackend.Batch{}
	index.Set(jmapIndexKey("Aalice", "x", "Thread", "example", "v", "T1"), nil)
	writes := store.writes
	checkError(t, b.WriteBatch(ctx, index))
	if store.writes != writes {
		t.Fatal("index-only batch wrote to S3")
	}
	// A one-record update never lists or rewrites unrelated owner records.
	lists = store.lists
	writes = store.writes
	failed.Set(userKey, []byte("last"))
	checkError(t, b.WriteBatch(ctx, failed))
	if store.lists != lists || store.writes-writes > 5 {
		t.Fatalf("update amplified: lists=%d writes=%d", store.lists-lists, store.writes-writes)
	}
}

func BenchmarkJMAPMetadataGet(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		for _, kind := range []string{"legacy", "owners"} {
			b.Run(fmt.Sprintf("%s/%d", kind, size), func(b *testing.B) {
				store := &memoryObjects{data: map[string][]byte{}}
				var backend jbackend.Backend
				if kind == "legacy" {
					backend = &jmapLegacyBackend{key: "t12e.cc/alice/.jmap/state.json", store: store}
				} else {
					backend = &jmapBackend{key: "t12e.cc/alice/.jmap/state.json", store: store}
				}
				ctx := context.Background()
				batch := &jbackend.Batch{}
				var key []byte
				for i := 0; i < size; i++ {
					key = jmapIndexKey("Aalice", "o", "Thread", fmt.Sprintf("T%d", i))
					batch.Set(key, []byte(`{"id":"thread","emailIds":["email"],"example":"metadata"}`))
				}
				if err := backend.WriteBatch(ctx, batch); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := backend.Get(ctx, key); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestJMAPOwnerSubmissionDiscoveryAfterRestart(t *testing.T) {
	base := &memoryObjects{data: map[string][]byte{}}
	ctx := context.Background()
	be := &jmapBackend{key: "t12e.cc/alice/.jmap/state.json", store: base}
	batch := &jbackend.Batch{}
	batch.Set(jmapIndexKey(string(jmapAccountID("t12e.cc/alice")), "o", "EmailSubmission", "S1"), []byte(`{"id":"S1","undoStatus":"pending","sendAt":"2050-01-01T00:00:00Z"}`))
	checkError(t, be.WriteBatch(ctx, batch))
	remote := &ownerTestStore{objectStore: base, versionedStore: base}
	a, err := newJMAPAccount("t12e.cc/alice", remote)
	checkError(t, err)
	accounts, err := a.db.TaggedAccounts(ctx, "mail:submission-queued")
	checkError(t, err)
	if !slices.Contains(accounts, a.id) {
		t.Fatal("cold worker cannot discover durable submission")
	}
	roots, err := jmapAccountRoots(remote)
	checkError(t, err)
	if !slices.Contains(roots, "t12e.cc/alice") {
		t.Fatal("account prefix discovery failed")
	}
	keys, err := base.List(".jmap-queue/")
	checkError(t, err)
	if len(keys) != 0 {
		t.Fatal("queue discovery persisted an index")
	}
}

type ownerNodeStore struct {
	objectStore
	versionedStore
}

func TestJMAPOwnerConcurrentNodes(t *testing.T) {
	base := &memoryObjects{data: map[string][]byte{}}
	ctx := context.Background()
	initial := &jmapBackend{key: "t12e.cc/alice/.jmap/state.json", store: base}
	key := jmapIndexKey("Aalice", "q")
	record := jmapIndexKey("Aalice", "o", "Thread", "T1")
	batch := &jbackend.Batch{}
	batch.Set(key, jbackend.EncodeInt64(0))
	batch.Set(record, []byte(`{"counter":0}`))
	checkError(t, initial.WriteBatch(ctx, batch))
	var group sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			node := &jmapBackend{key: initial.key, store: &ownerNodeStore{objectStore: base, versionedStore: base}}
			for range 20 {
				committed := false
				for attempt := 0; attempt < 256; attempt++ {
					value, err := node.Get(ctx, key)
					if errors.Is(err, jbackend.ErrAssertFailed) {
						continue
					}
					if err != nil {
						failures <- err
						return
					}
					n, err := jbackend.DecodeInt64(value)
					if err != nil {
						failures <- err
						return
					}
					update := &jbackend.Batch{}
					update.Assert(key, value)
					update.Set(key, jbackend.EncodeInt64(n+1))
					update.Set(record, fmt.Appendf(nil, `{"counter":%d}`, n+1))
					err = node.WriteBatch(ctx, update)
					if errors.Is(err, jbackend.ErrAssertFailed) {
						continue
					}
					if err != nil {
						failures <- err
						return
					}
					committed = true
					break
				}
				if !committed {
					failures <- fmt.Errorf("concurrent commit retries exhausted")
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		checkError(t, err)
	}
	cold := &jmapBackend{key: initial.key, store: &ownerNodeStore{objectStore: base, versionedStore: base}}
	values, err := cold.MultiGet(ctx, [][]byte{key, record})
	checkError(t, err)
	n, err := jbackend.DecodeInt64(values[0])
	checkError(t, err)
	if n != 80 || string(values[1]) != `{"counter":80}` {
		t.Fatalf("lost/partial update: %d %s", n, values[1])
	}
}

func TestJMAPLegacyPartLocatorIsMemoryOnly(t *testing.T) {
	base := &memoryObjects{data: map[string][]byte{}}
	blobs := jmapBlobs{store: base}
	account := jmapAccountID("t12e.cc/alice")
	source := jmap.Id("G" + strings.Repeat("a", 43))
	partID := jmap.Id("G" + strings.Repeat("b", 43))
	checkError(t, base.Put("t12e.cc/alice/.jmap/blobs/"+string(source), []byte("Content-Type: text/plain\r\n\r\nhello")))
	writer := jmapPartWriter{store: blobs, account: account, id: partID, part: jmapPartManifest{Source: source, Size: 5}}
	_, err := writer.Commit()
	checkError(t, err)
	keys, err := base.List("t12e.cc/alice/")
	checkError(t, err)
	if len(keys) != 1 {
		t.Fatalf("locator persisted: %v", keys)
	}
	reader, size, err := blobs.Open(context.Background(), account, partID)
	checkError(t, err)
	data, err := io.ReadAll(reader)
	reader.Close()
	checkError(t, err)
	if size != 5 || string(data) != "hello" {
		t.Fatalf("bad legacy part: %d %q", size, data)
	}
}

func TestDomainConfigurationRemoved(t *testing.T) {
	for _, flag := range []string{"-domain", "-domains"} {
		if _, err := loadConfig([]string{flag, "example.com"}, func(string) string { return "" }); err == nil {
			t.Fatal("obsolete domain configuration accepted")
		}
	}
}

func multipleDomainTestStore(t *testing.T) {
	outboundTestDir(t)
	for _, domain := range []string{"example.com", "example.org"} {
		checkError(t, objects.Put(domain+"/alice/.kind", []byte("account")))
		checkError(t, objects.Put(domain+"/alice/.password", []byte(domain)))
		checkError(t, objects.Put(domain+"/alias/.kind", []byte("alias")))
		checkError(t, objects.Put(domain+"/alias/.alias", []byte("alice")))
	}
}

func TestMultipleDomainIdentityIsolation(t *testing.T) {
	multipleDomainTestStore(t)
	for _, domain := range []string{"example.com", "example.org"} {
		identity, err := authenticateAccount("ALICE@"+strings.ToUpper(domain), domain)
		checkError(t, err)
		if identity.RootID != domain+"/alice" {
			t.Fatal(identity)
		}
		alias, err := authenticateAccount("alias@"+domain, domain)
		checkError(t, err)
		if alias.RootID != identity.RootID || alias.LoginID != domain+"/alias" {
			t.Fatal(alias)
		}
	}
	for _, address := range []string{"alice@example.net", "example.com/alice", "../alice@example.com", "alice@mail.example.org", "alice@example.com/../example.org"} {
		if _, err := authenticateAccount(address, "example.com"); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	if _, err := authenticateAccount("alice@example.org", "example.com"); err == nil {
		t.Fatal("password crossed domain")
	}
	if localUser("alice") != "" {
		t.Fatal("ambiguous bare name accepted")
	}
	checkError(t, objects.Put("example.com/alias/.alias", []byte("alice@example.org")))
	if _, err := resolveIdentity("alias@example.com"); err == nil {
		t.Fatal("alias crossed domain")
	}
	session := &smtpSession{requireAuth: true}
	checkError(t, session.authenticate("", "alice@example.com", "example.com"))
	if session.Mail("alice@example.org", nil) == nil {
		t.Fatal("spoofed sender in another domain")
	}
	checkError(t, session.Mail("alice@example.com", nil))
	checkError(t, session.Rcpt("alice@example.org", nil))
	if !slices.Equal(session.recipients, []string{"example.org/alice"}) || len(session.remote) != 0 {
		t.Fatal("hosted recipient routed externally")
	}
	if session.Rcpt("missing@example.org", nil) == nil {
		t.Fatal("missing local account accepted")
	}
	if err := session.Rcpt("unsupported+tag@example.org", nil); err == nil {
		t.Fatal("unsupported local account fell back to relay")
	}
	for _, root := range []string{"example.com/alice", "example.org/alice"} {
		id := jmapAccountID(root)
		got, err := jmapRoot(id)
		checkError(t, err)
		if got != root {
			t.Fatal("account ID lost domain")
		}
	}
	for _, key := range []string{"example.com/alice/mail/a/message.eml", "example.org/alice/.jmap/blobs/Gx"} {
		bad, _ := json.Marshal(namedObjectReference{Key: "example.com/bob/mail/a/message.eml"})
		if _, err := decodeObjectReference(key, bad); err == nil {
			t.Fatal("reference escaped account")
		}
	}
	bad, _ := json.Marshal(namedObjectReference{Key: "example.org/alice/mail/a/message.eml"})
	if _, err := decodeObjectReference("example.com/alice/mail/a/ref", bad); err == nil {
		t.Fatal("reference crossed domain")
	}
}

func TestMultipleDomainSharedProtocolsAndRecovery(t *testing.T) {
	multipleDomainTestStore(t)
	old := useJMAP
	useJMAP = true
	t.Cleanup(func() { useJMAP = old })
	ctx := context.Background()
	body := "From: sender@outside.example\r\nTo: alice@example.org\r\nSubject: domain isolation\r\n\r\n" + strings.Repeat("hello\r\n", 100)
	session := &smtpSession{}
	checkError(t, session.Mail("sender@outside.example", nil))
	checkError(t, session.Rcpt("alice@example.org", nil))
	checkError(t, session.Data(strings.NewReader(body)))
	a, err := openJMAPAccount(ctx, "example.com/alice")
	checkError(t, err)
	b, err := openJMAPAccount(ctx, "example.org/alice")
	checkError(t, err)
	for _, account := range []*jmapAccount{a, b} {
		if account.CanSendAs(ctx, account.id, "alice@"+accountDomain(account.root)) != true {
			t.Fatal("own sender denied")
		}
		other := "alice@example.org"
		if account == b {
			other = "alice@example.com"
		}
		if account.CanSendAs(ctx, account.id, other) {
			t.Fatal("JMAP sender crossed domain")
		}
	}
	first, err := messages(a.root)
	checkError(t, err)
	second, err := messages(b.root)
	checkError(t, err)
	if len(first) != 0 || len(second) != 1 || second[0].Uid != 1 {
		t.Fatal("mailboxes not isolated")
	}
	refs, err := mailBlobRefs(ctx, b.root)
	checkError(t, err)
	r, _, err := openStoredMessage(ctx, second[0], refs)
	checkError(t, err)
	got, err := io.ReadAll(r)
	r.Close()
	checkError(t, err)
	if string(got) != body {
		t.Fatal("mail body differs")
	}
	// Explicit delivery between hosted domains must copy into the recipient's namespace.
	session = &smtpSession{requireAuth: true}
	checkError(t, session.authenticate("", "alice@example.com", "example.com"))
	checkError(t, session.Mail("alice@example.com", nil))
	checkError(t, session.Rcpt("alice@example.org", nil))
	checkError(t, session.Data(strings.NewReader(body)))
	second, err = messages(b.root)
	checkError(t, err)
	if len(second) != 2 || second[1].Uid != 2 {
		t.Fatal("cross-domain delivery lost mail")
	}
	checkError(t, removeMessage(b.root, 1))
	first, err = messages(a.root)
	checkError(t, err)
	if len(first) != 0 {
		t.Fatal("recipient deletion changed sender inbox")
	}
	roots, err := jmapAccountRoots(objects)
	checkError(t, err)
	if !slices.Equal(roots, []string{"example.com/alias", a.root, "example.org/alias", b.root}) {
		t.Fatal("account discovery", roots)
	}
	roots, err = jmapAccountRoots(objects, "example.org")
	checkError(t, err)
	if !slices.Equal(roots, []string{"example.org/alias", b.root}) {
		t.Fatal("domain discovery", roots)
	}
	remote := &ownerTestStore{objectStore: objects, versionedStore: objects.(versionedStore)}
	cold, err := newJMAPAccount(b.root, remote)
	checkError(t, err)
	result, err := cold.call(ctx, "Email/query", map[string]any{})
	checkError(t, err)
	if len(jvalue[[]jmap.Id](result, "ids")) != 1 {
		t.Fatal("domain recovery lost email")
	}
	// A valid login for one domain must not authorize another domain's upload URL.
	req := httptest.NewRequest(http.MethodPost, "http://localhost/upload/"+string(b.id), strings.NewReader("forbidden"))
	req.SetBasicAuth("alice@example.com", "example.com")
	rec := httptest.NewRecorder()
	serveJMAP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("cross-domain upload accepted: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://localhost/.well-known/jmap", nil)
	req.SetBasicAuth("alice@example.org", "example.org")
	rec = httptest.NewRecorder()
	serveJMAP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "https://mail.example.org") || !strings.Contains(rec.Body.String(), "alice@example.org") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	keys, err := objects.List("")
	checkError(t, err)
	for _, key := range keys {
		if !strings.HasPrefix(key, "example.com/") && !strings.HasPrefix(key, "example.org/") {
			t.Fatal("data escaped domains", key)
		}
	}
}

func TestMultipleDomainQueueAndLeaseIsolation(t *testing.T) {
	multipleDomainTestStore(t)
	old := useJMAP
	useJMAP = false
	t.Cleanup(func() { useJMAP = old })
	now := time.Now()
	a, err := lockBucket(objects, now, "example.com")
	checkError(t, err)
	b, err := lockBucket(objects, now, "example.org")
	checkError(t, err)
	if _, err := lockBucket(objects, now, "example.com"); !errors.Is(err, fs.ErrExist) {
		t.Fatal("domain lease not exclusive", err)
	}
	checkError(t, a.renew(now.Add(time.Second)))
	a.release(now.Add(2 * time.Second))
	if !b.valid(now.Add(2 * time.Second)) {
		t.Fatal("release crossed domains")
	}
	a, err = lockBucket(objects, now.Add(3*time.Second), "example.com")
	checkError(t, err)
	for _, domain := range []string{"example.com", "example.org"} {
		checkError(t, queueMail(domain+"/alice", "alice@"+domain, nil, []string{"remote@outside.example"}, []byte("From: alice@"+domain+"\r\n\r\nhello")))
	}
	var workers sync.WaitGroup
	var seen []string
	_, err = scanTasks(context.Background(), a, make(chan struct{}, 2), &workers, func(c *claimedTask) error { seen = append(seen, c.key); return nil })
	checkError(t, err)
	workers.Wait()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "example.com/.outbox/") {
		t.Fatal("scanner crossed domains", seen)
	}
	keys, err := objects.List("example.org/.outbox/")
	checkError(t, err)
	if len(keys) != 1 {
		t.Fatal(keys)
	}
	job, err := readJSON[*outboundJob](keys[0])
	checkError(t, err)
	if job.Preclaim != nil {
		t.Fatal("other domain task was claimed")
	}
}

func TestDomainPrefixDiscoveryWithoutConfiguration(t *testing.T) {
	outboundTestDir(t)
	// Old bucket-root credentials never serve as an implicit default domain.
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("old")))
	if authenticate("alice", "old") || authenticate("alice@t12e.cc", "old") {
		t.Fatal("old bucket layout accepted")
	}
	if root, err := deliveryUser("alice@new.example"); err != nil || root != "" {
		t.Fatal(root, err)
	}
	checkError(t, objects.Put("new.example/alice/.kind", []byte("account")))
	checkError(t, objects.Put("new.example/alice/.password", []byte("new")))
	if !authenticate("alice@new.example", "new") {
		t.Fatal("new domain requires restart")
	}
	root, err := deliveryUser("missing@new.example")
	checkError(t, err)
	if root != "new.example/missing" {
		t.Fatal("hosted missing account would be routed externally")
	}
	domains, err := domainNamespaces()
	checkError(t, err)
	if !slices.Contains(domains, "new.example") {
		t.Fatal("scanner discovery missed new domain", domains)
	}
	checkError(t, objects.Delete("new.example/alice/.kind"))
	checkError(t, objects.Delete("new.example/alice/.password"))
	if root, err := deliveryUser("alice@new.example"); err != nil || root != "" {
		t.Fatal("removed domain remained hosted", root, err)
	}
}
