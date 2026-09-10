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
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/emersion/go-imap"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	jmap "github.com/naust-mail/naust-jmap/core/jmap"
	jbackend "github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
	"slices"
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
	paths, err := objects.List(outbox + "/")
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
	checkError(t, queueMail("jw238", "jw238@t12e.cc", nil, []string{"recipient@example.net"}, body))
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
	checkError(t, queueMail("jw238", "jw238@t12e.cc", nil, []string{"missing@example.net"}, []byte("Subject: test\r\n\r\nbody\r\n")))
	path, _ := queuedJob(t)
	claimAndProcess(t, path)
	if _, err := objects.Get(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed terminal task/preclaim remained", err)
	}
	notices, err := messages("jw238")
	if err != nil || len(notices) != 1 || !strings.Contains(string(notices[0].Body), "missing@example.net") {
		t.Fatal("expected one local failure notice", err)
	}
}
func TestExternalRecipientRequiresAuthentication(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("jw238/.kind", []byte("account")))
	checkError(t, objects.Put("jw238/.password", []byte("123123")))
	session := &smtpSession{}
	if session.Rcpt("recipient@example.net", nil) == nil {
		t.Fatal("open relay")
	}
	session.user = "jw238"
	checkError(t, session.Mail("jw238@t12e.cc", nil))
	checkError(t, session.Rcpt("recipient@example.net", nil))
	if err := session.Rcpt("unknown@t12e.cc", nil); err == nil {
		t.Fatal("unknown local recipient sent externally")
	}
	session.Reset()
	if len(session.remote) != 0 || session.from != "" || session.user != "jw238" {
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
	checkError(t, queueMail("jw238", "jw238@t12e.cc", nil, []string{"recipient@example.net"}, []byte("Subject: saved\r\n\r\nbody\r\n")))
	saved, err := messages(testFolderKey(t, "jw238", "Sent"))
	if err != nil || len(saved) != 1 {
		t.Fatal("Sent missing", err)
	}
	checkError(t, deleteMessages(testFolderKey(t, "jw238", "Sent"), []uint32{saved[0].Uid}))
	_, job := queuedJob(t)
	if !job.Archived {
		t.Fatal("Sent archive status not saved in task")
	}
	saved, err = messages(testFolderKey(t, "jw238", "Sent"))
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
	c, err := loadConfig([]string{"-domain", "example.org", "-outbound", "direct", "-queue-retry", "2s", "-smtps", "127.0.0.1:2465"}, func(key string) string {
		calls[key]++
		if key == "FMA_OUTBOUND_MODE" {
			return "relay"
		}
		return ""
	})
	checkError(t, err)
	if c.Domain != "example.org" || c.OutboundMode != "direct" || c.QueueRetry != 2*time.Second || c.SMTPSAddr != "127.0.0.1:2465" || c.POP3SAddr != "127.0.0.1:1995" {
		t.Fatal("configuration defaults or flag precedence changed")
	}
	if len(calls) != 14 {
		t.Fatalf("expected 14 environment inputs, got %d", len(calls))
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
	checkError(t, queueMail("jw238", "jw238@t12e.cc", []string{"jw238"}, nil, local))
	checkError(t, queueMail("jw238", "jw238@t12e.cc", []string{"jw238"}, []string{"recipient@example.net"}, remote))
	for _, key := range []string{"jw238", testFolderKey(t, "jw238", "Sent")} {
		mail, err := messages(key)
		if err != nil || len(mail) != 2 || string(mail[0].Body) != string(local) || string(mail[1].Body) != string(remote) {
			t.Fatalf("shared storage %s: count=%d err=%v", key, len(mail), err)
		}
	}
	_, job := queuedJob(t)
	if len(job.Recipients) != 1 || job.Recipients[0].State != "pending" || string(job.Body) != string(remote) {
		t.Fatal("queue lost recipient state or original body")
	}
	checkError(t, saveSent("jw238", remote))
	sent, err := messages(testFolderKey(t, "jw238", "Sent"))
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
			release, err := lockBucket(store, time.Now())
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
	release, err := lockBucket(store, time.Now())
	checkError(t, err)
	release.release(time.Now())
}
func TestFoldersRenameAndRecreate(t *testing.T) {
	outboundTestDir(t)
	user := &imapUser{name: "alice"}
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
	if authenticate("alice", "secret") {
		t.Fatal("missing account accepted")
	}
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("secret")))
	if !authenticate("alice", "secret") {
		t.Fatal("external account was not discovered")
	}

	checkError(t, deliver([]string{"alice"}, []byte("Subject: inbox\r\n\r\nbody")))
	checkError(t, saveSent("alice", []byte("Subject: sent\r\n\r\nbody")))
	inboxMessages, err := messages("alice")
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
	user := &imapUser{name: "alice"}
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
	first, err := lockBucket(objects, now)
	checkError(t, err)
	checkError(t, first.renew(now.Add(15*time.Second)))
	record, _, err := readLock(objects.(versionedStore))
	checkError(t, err)
	if !record.StartedAt.Equal(now) || !record.RenewedAt.Equal(now.Add(15*time.Second)) || !record.ExpiresAt.Equal(now.Add(45*time.Second)) {
		t.Fatal("incorrect lease timestamps")
	}
	if _, err := lockBucket(objects, now.Add(31*time.Second)); err == nil {
		t.Fatal("renewed lease stolen")
	}
	second, err := lockBucket(objects, now.Add(46*time.Second))
	checkError(t, err)
	if first.renew(now.Add(47*time.Second)) == nil {
		t.Fatal("expired owner renewed")
	}
	first.release(now.Add(47 * time.Second))
	record, _, err = readLock(objects.(versionedStore))
	checkError(t, err)
	if record.Owner != second.record.Owner {
		t.Fatal("stale release changed successor lock")
	}
	second.release(now.Add(47 * time.Second))
	third, err := lockBucket(objects, now.Add(48*time.Second))
	checkError(t, err)
	third.release(now.Add(49 * time.Second))
}
func TestExpiredLeaseTakeoverRace(t *testing.T) {
	outboundTestDir(t)
	now := time.Now()
	_, err := lockBucket(objects, now.Add(-time.Minute))
	checkError(t, err)
	var wg sync.WaitGroup
	var winners atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := lockBucket(objects, now); err == nil {
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
	lease, err := lockBucket(objects, time.Now())
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
	if authenticate("alice", "secret") {
		t.Fatal("missing account accepted")
	}
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("secret\n")))
	if !authenticate("alice@t12e.cc", "secret") || authenticate("alice", "wrong") {
		t.Fatal("new account authentication failed")
	}
	session := &smtpSession{}
	checkError(t, session.Rcpt("alice@t12e.cc", nil))
	if session.Rcpt("alicex@t12e.cc", nil) == nil {
		t.Fatal("prefix isolation failed")
	}
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("changed")))
	if authenticate("alice", "secret") || !authenticate("alice", "changed") {
		t.Fatal("password change required reload")
	}
	checkError(t, objects.Delete("alice/.password"))
	if authenticate("alice", "changed") || session.Rcpt("alice@t12e.cc", nil) == nil {
		t.Fatal("deleted account still accepted")
	}
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", nil))
	if authenticate("alice", "") || session.Rcpt("alice@t12e.cc", nil) == nil {
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
	checkError(t, queueMail("alice", "alice@t12e.cc", nil, []string{"r@example.net"}, []byte("Subject: claim\r\n\r\nbody")))
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
	if c, err := preclaimTask(key, "other", old.expires.Add(-time.Nanosecond)); err != nil || c != nil {
		t.Fatal("live claim stolen", err)
	}
	current, err := preclaimTask(key, "other", old.expires)
	checkError(t, err)
	if current == nil || current.job.Preclaim.Owner != "other" {
		t.Fatal("expired preclaim not recovered")
	}
	old.job.Complete = true
	old.job.Preclaim = nil
	if old.save(context.Background()) == nil {
		t.Fatal("old worker overwrote successor")
	}
	stored, err := readJSON[*outboundJob](key)
	checkError(t, err)
	if stored.Complete || stored.Preclaim.Owner != "other" {
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
	checkError(t, queueMail("alice", "alice@t12e.cc", nil, []string{"r@example.net"}, []byte("Subject: cleanup\r\n\r\nbody")))
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
			if err := ensureFolders("alice"); err != nil {
				failures <- err
				return
			}
			failures <- appendMessage("alice", []byte(fmt.Sprintf("message %d", i)), nil, time.Now(), false)
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		checkError(t, err)
	}
	mail, err := messages("alice")
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
		key := fmt.Sprintf("%s/%032x.json", outbox, i)
		checkError(t, writeJSON(key, &outboundJob{ID: fmt.Sprintf("%032x", i), Archived: true, Recipients: []outboundRecipient{{Address: "a@example.net", State: "pending"}}}))
	}
	lease, err := lockBucket(objects, time.Now())
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
	next, err := lockBucket(objects, time.Now())
	checkError(t, err)
	defer next.release(time.Now())
	key := fmt.Sprintf("%s/%032x.json", outbox, maxClaimedTasks)
	last, err := preclaimTask(key, next.owner(), time.Now())
	checkError(t, err)
	if last == nil {
		t.Fatal("next node could not claim remaining task")
	}
	first, err := preclaimTask(fmt.Sprintf("%s/%032x.json", outbox, 0), next.owner(), time.Now())
	checkError(t, err)
	if first != nil {
		t.Fatal("releasing scanner lock released a live task")
	}
}

func TestPreclaimExecutionCancellationPreservesTask(t *testing.T) {
	outboundTestDir(t)
	checkError(t, queueMail("alice", "alice@t12e.cc", nil, []string{"a@example.net"}, []byte("Subject: cancel\r\n\r\nbody")))
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
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("secret")))
	checkError(t, objects.Put("sales/.kind", []byte("alias")))
	checkError(t, objects.Put("sales/.alias", []byte("alice\n")))
	checkError(t, objects.Put("sales/.password", []byte("ignored")))
	checkError(t, objects.Put("alice/.profile.json", []byte(`{"display_name":"Alice","avatar":"avatar.png"}`)))
	identity, err := authenticateAccount("sales@t12e.cc", "secret")
	checkError(t, err)
	if identity.LoginID != "sales" || identity.RootID != "alice" || authenticate("sales", "ignored") {
		t.Fatal("alias identity or credentials incorrect")
	}
	sender := &smtpSession{requireAuth: true}
	checkError(t, sender.authenticate("alice", "sales", "secret"))
	checkError(t, sender.Mail("sales@t12e.cc", nil))
	checkError(t, sender.Rcpt("alice@t12e.cc", nil))
	checkError(t, sender.Rcpt("sales@t12e.cc", nil))
	if len(sender.recipients) != 1 {
		t.Fatal("root and alias were not deduplicated")
	}
	checkError(t, sender.Data(strings.NewReader("Subject: aliases\r\n\r\nbody\r\n")))
	mailbox, err := messages("alice")
	checkError(t, err)
	if len(mailbox) != 1 {
		t.Fatal("metadata treated as mail or duplicate delivery")
	}
	aliasMail, err := messages("sales")
	checkError(t, err)
	if len(aliasMail) != 0 {
		t.Fatal("mail stored under alias prefix")
	}
	user, err := (imapBackend{}).Login(nil, "sales", "secret")
	checkError(t, err)
	if user.Username() != "sales" || user.(*imapUser).name != "alice" {
		t.Fatal("IMAP lost login or root ID")
	}
	pop := &popMailbox{deleted: map[int]bool{}}
	checkError(t, pop.Login(context.Background(), "sales", "secret"))
	defer pop.Close()
	second := &popMailbox{deleted: map[int]bool{}}
	if second.Login(context.Background(), "alice", "secret") == nil {
		second.Close()
		t.Fatal("alias bypassed root maildrop lock")
	}
	if pop.loginID != "sales" || pop.user != "alice" {
		t.Fatal("POP identity incorrect")
	}
	checkError(t, pop.Dele(context.Background(), 1))
	checkError(t, pop.Close())
	mailbox, err = messages("alice")
	checkError(t, err)
	if len(mailbox) != 1 {
		t.Fatal("disconnect committed alias deletes")
	}
	checkError(t, objects.Put("other/.kind", []byte("account")))
	checkError(t, objects.Put("other/.password", []byte("different")))
	checkError(t, objects.Put("sales/.kind", []byte("alias")))
	checkError(t, objects.Put("sales/.alias", []byte("other")))
	if authenticate("sales", "secret") || !authenticate("sales", "different") {
		t.Fatal("alias target change not immediate")
	}
	if sender.Mail("sales@t12e.cc", nil) == nil {
		t.Fatal("old session impersonated new alias owner")
	}
}

func TestInvalidAliasesFailClosed(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("secret")))
	for _, target := range []string{"", "missing", "../alice", "alice@t12e.cc", "sales"} {
		checkError(t, objects.Put("sales/.kind", []byte("alias")))
		checkError(t, objects.Put("sales/.alias", []byte(target)))
		if authenticate("sales", "secret") {
			t.Fatalf("invalid alias accepted: %q", target)
		}
		if (&smtpSession{}).Rcpt("sales@t12e.cc", nil) == nil {
			t.Fatal("invalid alias recipient accepted")
		}
	}
	checkError(t, objects.Put("sales/.kind", []byte("alias")))
	checkError(t, objects.Put("sales/.alias", []byte("support")))
	checkError(t, objects.Put("support/.kind", []byte("alias")))
	checkError(t, objects.Put("support/.alias", []byte("sales")))
	if authenticate("sales", "secret") {
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
		func(c *Config) { c.S3.Endpoint = "ftp://localhost" }, func(c *Config) { c.Domain = "" },
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
	c.Domain = ""
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
		"slot/.password": "slot-password", "slot/.alias": "root", "slot/.proxy": "remote@example.net",
		"root/.kind": "account", "root/.password": "root-password",
	} {
		checkError(t, objects.Put(key, []byte(value)))
	}
	for _, kind := range []string{"", "unknown", "account alias", "ACCOUNT"} {
		checkError(t, objects.Put("slot/.kind", []byte(kind)))
		if authenticate("slot", "slot-password") || (&smtpSession{}).Rcpt("slot@t12e.cc", nil) == nil {
			t.Fatal("invalid kind accepted", kind)
		}
	}
	checkError(t, objects.Delete("slot/.kind"))
	if authenticate("slot", "slot-password") {
		t.Fatal("missing kind inferred from password")
	}
	checkError(t, objects.Put("slot/.kind", []byte("account\n")))
	if !authenticate("slot", "slot-password") || authenticate("slot", "root-password") {
		t.Fatal("account used alias or proxy config")
	}
	account := &smtpSession{}
	checkError(t, account.Rcpt("slot@t12e.cc", nil))
	if len(account.remote) != 0 || len(account.recipients) != 1 || account.recipients[0] != "slot" {
		t.Fatal("account routed via stale proxy")
	}
	checkError(t, objects.Put("slot/.kind", []byte("alias")))
	if !authenticate("slot", "root-password") || authenticate("slot", "slot-password") {
		t.Fatal("alias used stale password")
	}
	checkError(t, objects.Put("slot/.kind", []byte("proxy")))
	if authenticate("slot", "slot-password") || authenticate("slot", "root-password") {
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
		"forward/.kind": "proxy", "forward/.proxy": "alias@t12e.cc",
		"alias/.kind": "alias", "alias/.alias": "root",
		"root/.kind": "account", "root/.password": "secret",
	} {
		checkError(t, objects.Put(key, []byte(value)))
	}
	body := []byte("From: sender@example.net\r\nSubject: local proxy\r\n\r\noriginal\r\n")
	session := &smtpSession{}
	checkError(t, session.Mail("sender@example.net", nil))
	checkError(t, session.Rcpt("forward@t12e.cc", nil))
	checkError(t, session.Rcpt("root@t12e.cc", nil))
	checkError(t, session.Data(bytes.NewReader(body)))
	mailbox, err := messages("root")
	checkError(t, err)
	if len(mailbox) != 1 || !bytes.Equal(mailbox[0].Body, body) {
		t.Fatal("local proxy lost or duplicated mail")
	}
	mailbox, err = messages("forward")
	checkError(t, err)
	if len(mailbox) != 0 {
		t.Fatal("proxy kept a local copy")
	}
	for _, target := range []string{"forward@t12e.cc", "bad", "one@example.net,two@example.net", "name <one@example.net>", "../root@t12e.cc", "missing@t12e.cc", "remote@example.net"} {
		checkError(t, objects.Put("forward/.proxy", []byte(target)))
		if (&smtpSession{}).Rcpt("forward@t12e.cc", nil) == nil {
			t.Fatal("invalid/disabled proxy accepted", target)
		}
	}
	checkError(t, objects.Put("forward/.proxy", []byte("alias@t12e.cc")))
	checkError(t, objects.Put("alias/.alias", []byte("forward")))
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
			for _, id := range []string{"first", "second"} {
				checkError(t, objects.Put(id+"/.kind", []byte("proxy")))
				checkError(t, objects.Put(id+"/.proxy", []byte("destination@example.net")))
			}
			body := []byte("From: sender@example.net\r\nTo: first@t12e.cc\r\nSubject: forwarded\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAwQF\r\n")
			s := &smtpSession{}
			checkError(t, s.Mail("sender@example.net", nil))
			checkError(t, s.Rcpt("first@t12e.cc", nil))
			checkError(t, s.Rcpt("second@t12e.cc", nil))
			// Retargeting after RCPT must not alter the accepted routing snapshot.
			checkError(t, objects.Put("first/.proxy", []byte("changed@example.net")))
			checkError(t, s.Data(bytes.NewReader(body)))
			key, job := queuedJob(t)
			if job.User != "" || job.From != "sender@example.net" || len(job.Recipients) != 1 || job.Recipients[0].Address != "destination@example.net" || len(job.Recipients[0].ProxyOwners) != 2 {
				t.Fatal("proxy routing was not persisted")
			}
			if !bytes.Equal(job.Body, append([]byte("X-FMA-Proxy-Hops: 1\r\n"), body...)) {
				t.Fatal("forwarding changed MIME content")
			}
			for _, id := range []string{"first", "second"} {
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
				for _, id := range []string{"first", "second"} {
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
			return &jmapBackend{key: b.key, store: b.store}
		},
	})
}

func TestJMAPSharedMailbox(t *testing.T) {
	outboundTestDir(t)
	checkError(t, objects.Put("alice/.kind", []byte("account")))
	checkError(t, objects.Put("alice/.password", []byte("secret")))
	body := []byte("From: alice@t12e.cc\r\nTo: alice@t12e.cc\r\nSubject: shared\r\nMessage-ID: <shared@t12e.cc>\r\n\r\nhello\r\n")
	checkError(t, appendMessage("alice", body, nil, time.Now(), false))
	old := useJMAP
	useJMAP = true
	t.Cleanup(func() { useJMAP = old })
	a, err := openJMAPAccount(context.Background(), "alice")
	checkError(t, err)
	result, err := a.call(context.Background(), "Email/query", map[string]any{})
	checkError(t, err)
	ids := jvalue[[]jmap.Id](result, "ids")
	if len(ids) != 1 {
		t.Fatalf("bootstrap ids: %s", result)
	}
	stored, err := messages("alice")
	checkError(t, err)
	if len(stored) != 1 || stored[0].Uid != 1 || stored[0].Body != nil {
		t.Fatalf("shared mailbox: %+v", stored)
	}
	refs, err := mailBlobRefs(context.Background(), "alice")
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
	stored, err = messages("alice")
	checkError(t, err)
	if !slices.Contains(stored[0].Flags, imap.SeenFlag) {
		t.Fatal("JMAP flags not visible in IMAP")
	}
	checkError(t, appendMessage("alice", body, nil, time.Now(), false))
	stored, err = messages("alice")
	checkError(t, err)
	if len(stored) != 2 || stored[1].Uid != 2 {
		t.Fatalf("append: %+v", stored)
	}
	checkError(t, removeMessage("alice", 1))
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
	root := fmt.Sprintf("compression%d", time.Now().UnixNano())
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
			// Cross-account/server-side copy must preserve compression metadata.
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
		w := &jmapBlobWriter{ctx: context.Background(), account: jmapAccountID("bench"), digest: sha256.New(), writer: discardObjectUpload{}}
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
		w := &jmapBlobWriter{ctx: context.Background(), account: jmapAccountID("test"), digest: sha256.New(), writer: discardObjectUpload{}}
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
