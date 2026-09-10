#!/usr/bin/env python3
"""Protocol integration checks. Default: isolated local server. --host: deployed server."""
import argparse
import base64
from email.message import EmailMessage
from email.parser import BytesParser
from email import policy
import concurrent.futures
import imaplib
import os
from pathlib import Path
import poplib
from contextlib import closing
import signal
import smtplib
import socket
import ssl
import subprocess
import tempfile
import time
import uuid
import urllib.request
import xml.etree.ElementTree as ET
from verify_folders import verify_folders
from verify_jmap import verify_jmap, verify_jmap_restart


def require(value, message):
    if not value:
        raise AssertionError(message)


def verify(host, ports, context, user='jw238', password='123123', isolated=False, skip_inbound=False):
    inbound, submission, smtps, pop, imap = ports[:5]
    recipient = user + '@t12e.cc'
    marker = 'fma-verify-' + uuid.uuid4().hex
    payload = (f'From: {recipient}\r\nTo: {recipient}\r\nSubject: {marker}\r\n'
               f'Message-ID: <{marker}@t12e.cc>\r\n\r\nFirst line\r\n.dot stuffed\r\n中文邮件\r\n').encode()
    with smtplib.SMTP(host, submission, timeout=15) as s:
        s.ehlo()
        require(not s.has_extn('auth'), 'AUTH must not be advertised before TLS')
        require(s.mail(recipient)[0] == 530, 'submission must require authentication')
        s.starttls(context=context)
        try:
            s.login(user, 'wrong-password')
            raise AssertionError('bad SMTP password accepted')
        except smtplib.SMTPAuthenticationError:
            pass
        s.login(user, password)
        require(s.mail(recipient)[0] == 250, 'authenticated MAIL failed')
        external_code = s.rcpt('someone@example.net')[0]
        require(external_code == 451 if isolated else external_code in (250, 451), 'unexpected authenticated outbound policy')
        s.rset()
        require(s.sendmail(recipient, [recipient], payload) == {}, 'SMTP delivery failed')
    with smtplib.SMTP_SSL(host, smtps, timeout=15, context=context) as s:
        s.login(recipient, password)
        s.sendmail(recipient, [recipient], payload.replace(b'First line', b'SMTPS line'))
    if not skip_inbound:
        with smtplib.SMTP(host, inbound, timeout=15) as s:
            s.ehlo()
            require(s.mail('outside@example.net')[0] == 250, 'inbound rejected external sender')
            require(s.rcpt('unknown-user@t12e.cc')[0] == 550, 'unknown user accepted')
            require(s.rcpt('outside@example.net')[0] == 550, 'inbound open relay')
            s.rset()
            s.sendmail('outside@example.net', [recipient], payload.replace(b'First line', b'Inbound line'))
    print('PASS SMTP STARTTLS / AUTH PLAIN / SMTPS / relay rejection')
    print('SKIP public inbound port 25' if skip_inbound else 'PASS inbound SMTP port 25')
    expected = 2 if skip_inbound else 3

    def open_imap():
        c = imaplib.IMAP4_SSL(host, imap, ssl_context=context, timeout=15)
        c.login(user, password)
        require(c.list()[0] == 'OK', 'IMAP LIST failed')
        require(c.select('INBOX', readonly=True)[0] == 'OK', 'IMAP SELECT failed')
        return c

    bad = imaplib.IMAP4_SSL(host, imap, ssl_context=context, timeout=15)
    try:
        bad.login(user, 'wrong-password')
        raise AssertionError('bad IMAP password accepted')
    except imaplib.IMAP4.error:
        pass
    finally:
        bad.logout()
    c = open_imap()
    status, results = c.uid('search', None, 'HEADER', 'Subject', marker)
    ids = results[0].split()
    require(status == 'OK' and len(ids) == expected, f'IMAP SEARCH expected {expected}: {results}')
    status, fetched = c.uid('fetch', ids[0], '(UID RFC822.SIZE BODY.PEEK[])')
    require(status == 'OK' and any(isinstance(x, tuple) and x[1] == payload for x in fetched), 'IMAP body mismatch')
    all_uids = c.uid('search', None, 'ALL')[1][0].split()
    require(c.uid('search', None, 'UID', '*')[1][0].split() == all_uids[-1:], 'UID star search broken')
    status, fetched = c.uid('fetch', '*', '(UID BODY.PEEK[])')
    require(status == 'OK' and len([x for x in fetched if isinstance(x, tuple)]) == 1, 'UID star fetch broken')
    before_next = int(c.status('INBOX', '(UIDNEXT)')[1][0].split(b'UIDNEXT ')[1].split(b')')[0])
    c.logout()
    assert c.state == 'LOGOUT'
    sent_client = open_imap()
    sent_client.select('Sent', readonly=True)
    assert sent_client.uid('search', None, 'HEADER', 'Subject', marker)[1][0], 'SMTP Sent archive missing'
    sent_client.logout()
    print('PASS IMAPS login / LIST / SELECT / UID SEARCH / FETCH')

    def open_pop():
        p = poplib.POP3_SSL(host, pop, timeout=15, context=context)
        p.user(user)
        p.pass_(password)
        return p

    bad = poplib.POP3_SSL(host, pop, timeout=15, context=context)
    bad.user(user)
    try:
        bad.pass_('wrong-password')
        raise AssertionError('bad POP password accepted')
    except poplib.error_proto:
        pass
    finally:
        bad.close()
    p = open_pop()
    locked = poplib.POP3_SSL(host, pop, timeout=15, context=context)
    locked.user(user)
    try:
        locked.pass_(password)
        raise AssertionError('concurrent POP transaction accepted')
    except poplib.error_proto:
        pass
    finally:
        locked.close()
    require('UIDL' in p.capa(), 'POP capability missing')
    listings = p.list()[1]
    require(all(int(x.split()[0]) > 0 for x in listings), 'POP sequence starts at zero')
    marked = []
    for entry in listings:
        n = int(entry.split()[0])
        response, lines, _ = p.retr(n)
        body = b'\r\n'.join(lines) + b'\r\n'
        if marker.encode() in body:
            marked.append(n)
            require(b'\r\n.dot stuffed\r\n' in body, 'POP dot stuffing broken')
            require(len(body) == int(entry.split()[1]), 'POP LIST size mismatch')
            require(marker.encode() in b'\n'.join(p.top(n, 0)[1]), 'POP TOP failed')
    require(len(marked) == expected, 'POP and IMAP do not share storage')
    for n in marked:
        p.dele(n)
    p.rset()
    require(all(any(int(x.split()[0]) == n for x in p.list()[1]) for n in marked), 'RSET did not restore messages')
    p.dele(marked[0])
    p.close()  # No QUIT: deletion must roll back, mailbox lock must release.
    time.sleep(.15)
    p = open_pop()
    require(marker.encode() in b'\n'.join(p.retr(marked[0])[1]), 'disconnect committed deletion')
    for n in marked:
        p.dele(n)
    p.quit()
    c = open_imap()
    require(c.uid('search', None, 'HEADER', 'Subject', marker)[1] == [b''], 'POP deletion not reflected in IMAP')
    after_next = int(c.status('INBOX', '(UIDNEXT)')[1][0].split(b'UIDNEXT ')[1].split(b')')[0])
    require(after_next >= before_next, 'UIDNEXT went backwards after deletion')
    c.logout()
    print('PASS POP3S download / TOP / UIDL / RSET / disconnect rollback / QUIT deletion')
    plain_pop = poplib.POP3(host, ports[6], timeout=15)
    require('STLS' in plain_pop.capa(), 'POP STLS capability missing')
    try:
        plain_pop.user(user)
        plain_pop.pass_(password)
        raise AssertionError('POP accepted credentials before TLS')
    except poplib.error_proto:
        pass
    plain_pop.stls(context=context)
    require('STLS' not in plain_pop.capa(), 'POP advertises STLS after TLS')
    plain_pop.user(user)
    plain_pop.pass_(password)
    plain_pop.stat()
    plain_pop.quit()
    sasl_pop = poplib.POP3_SSL(host, pop, timeout=15, context=context)
    require('PLAIN' in sasl_pop.capa().get('SASL', []), 'POP SASL PLAIN capability missing')
    token = base64.b64encode(('\0' + user + '\0' + password).encode()).decode()
    sasl_pop._shortcmd('AUTH PLAIN ' + token)
    sasl_pop.stat()
    sasl_pop.quit()
    plain_imap = imaplib.IMAP4(host, ports[7], timeout=15)
    require('STARTTLS' in plain_imap.capabilities, 'IMAP STARTTLS capability missing')
    try:
        plain_imap.login(user, password)
        raise AssertionError('IMAP accepted credentials before TLS')
    except imaplib.IMAP4.error:
        pass
    plain_imap.starttls(ssl_context=context)
    plain_imap.login(user, password)
    require(plain_imap.select('INBOX', readonly=True)[0] == 'OK', 'IMAP STARTTLS mailbox failed')
    plain_imap.logout()
    print('PASS POP3 STLS / IMAP STARTTLS / plaintext authentication rejected')
    if isolated:
        p = poplib.POP3_SSL(host, pop, timeout=15, context=context)
        p.user('jw238x')
        p.pass_('different-password')
        require(p.stat()[0] == 0, 'prefix user can read another mailbox')
        p.quit()
        with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
            def send(i):
                with smtplib.SMTP_SSL(host, smtps, context=context, timeout=15) as s:
                    s.login(user, password)
                    s.sendmail(recipient, [recipient], payload.replace(marker.encode(), f'concurrent-{i}'.encode()))
            list(pool.map(send, range(12)))
        c = open_imap()
        require(len(c.search(None, 'ALL')[1][0].split()) == 12, 'concurrent mail lost')
        c.logout()
        print('PASS user isolation / concurrent delivery')


def attachment_message():
    msg = EmailMessage(policy=policy.SMTP)
    msg['From'] = 'jw238@t12e.cc'
    msg['To'] = 'mime-to@t12e.cc, mime-header-only@t12e.cc'
    msg['Cc'] = 'mime-cc@t12e.cc'
    msg['Bcc'] = 'mime-bcc@t12e.cc'
    msg['Subject'] = 'MIME attachments and envelope recipients'
    msg.set_content('中文正文\n.dot stuffed\n', cte='quoted-printable')
    msg.add_alternative('<html><body>中文<img src="cid:logo"></body></html>', subtype='html')
    msg.get_payload()[1].add_related(b'\x89PNG\r\n\x00\xff', maintype='image', subtype='png', cid='<logo>', filename='logo.png', disposition='inline')
    msg.add_attachment(bytes(range(256)) * 4096, maintype='application', subtype='octet-stream', filename='报告-完整数据.bin')
    msg.add_attachment(b'', maintype='application', subtype='octet-stream', filename='empty.bin')
    return msg


def verify_attachment_mailboxes(host, ports, context, expected):
    for user in ['mime-to', 'mime-cc', 'mime-bcc']:
        with imaplib.IMAP4_SSL(host, ports[4], ssl_context=context, timeout=15) as c:
            c.login(user, 'mime-password')
            c.select('INBOX', readonly=True)
            ids = c.uid('search', None, 'ALL')[1][0].split()
            require(len(ids) == 1, 'To/Cc/Bcc recipient missing or duplicate delivery: ' + user)
            status, data = c.uid('fetch', ids[0], '(RFC822.SIZE BODYSTRUCTURE BODY.PEEK[])')
            raw = next(item[1] for item in data if isinstance(item, tuple) and b"BODY[]" in item[0])
            require(status == 'OK' and raw == expected, 'IMAP changed MIME message: ' + user)
            structure = b' '.join(item[0] if isinstance(item, tuple) else item for item in data if item)
            require(b'ATTACHMENT' in structure.upper() and b'BASE64' in structure.upper(), 'IMAP attachment BODYSTRUCTURE missing')
            parsed = BytesParser(policy=policy.default).parsebytes(raw)
            require(parsed['Cc'] == 'mime-cc@t12e.cc' and parsed['Bcc'] is None, 'Cc/Bcc header privacy broken')
            parts = {part.get_filename(): part for part in parsed.walk() if part.get_filename()}
            require(set(parts) == {'报告-完整数据.bin', 'empty.bin', 'logo.png'}, 'attachment filenames changed')
            require(parts['报告-完整数据.bin'].get_payload(decode=True) == bytes(range(256)) * 4096, 'binary attachment corrupted')
            require(parts['empty.bin'].get_payload(decode=True) == b'', 'empty attachment corrupted')
            require(parts['logo.png']['Content-ID'] == '<logo>' and parts['logo.png'].get_payload(decode=True) == b'\x89PNG\r\n\x00\xff', 'inline image corrupted')
            status, section = c.uid('fetch', ids[0], '(BODY.PEEK[2])')
            encoded = next(item[1] for item in section if isinstance(item, tuple))
            require(status == 'OK' and base64.b64decode(encoded) == bytes(range(256)) * 4096, 'IMAP attachment section corrupted')
            status, partial = c.uid('fetch', ids[0], '(BODY.PEEK[2]<0.64>)')
            require(status == 'OK' and next(item[1] for item in partial if isinstance(item, tuple)) == encoded[:64], 'partial attachment fetch corrupted')
        with closing(poplib.POP3_SSL(host, ports[3], context=context, timeout=15)) as p:
            p.user(user)
            p.pass_('mime-password')
            require(p.stat()[0] == 1, 'POP recipient count mismatch')
            raw = b'\r\n'.join(p.retr(1)[1]) + b'\r\n'
            require(raw == expected and p.stat()[1] == len(raw), 'POP attachment bytes/size mismatch')
    with imaplib.IMAP4_SSL(host, ports[4], ssl_context=context, timeout=15) as c:
        c.login('mime-header-only', 'mime-password')
        c.select('INBOX', readonly=True)
        require(c.uid('search', None, 'ALL')[1][0] == b'', 'header-only recipient received mail without RCPT TO')
    with imaplib.IMAP4_SSL(host, ports[4], ssl_context=context, timeout=15) as c:
        c.login('jw238', '123123')
        c.select('Sent', readonly=True)
        ids = c.uid('search', None, 'SUBJECT', '"MIME attachments and envelope recipients"')[1][0].split()
        require(len(ids) == 1, 'MIME Sent archive missing')
        data = c.uid('fetch', ids[0], '(BODY.PEEK[])')[1]
        require(next(item[1] for item in data if isinstance(item, tuple)) == expected, 'Sent attachment changed')
    print('PASS attachments / Unicode filenames / empty file / inline image / MIME sections / To-Cc-Bcc / envelope-only delivery')


def local():
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix='fma-mail-test-') as tmp:
        tmp = Path(tmp)
        binary = tmp / 'fma'
        subprocess.run(['go', 'build', '-race', '-ldflags', os.environ.get('FMA_BUILD_LDFLAGS', ''), '-o', str(binary), '.'], cwd=root, check=True)
        # The S3 service owns its filesystem; the mail process gets an empty,
        # read-only working directory and only a bucket connection.
        s3_socket = socket.socket()
        s3_socket.bind(('127.0.0.1', 0))
        s3_port = s3_socket.getsockname()[1]
        s3_socket.close()
        endpoint = f'http://127.0.0.1:{s3_port}'
        fals3y = os.environ.get('FALS3Y_BIN', str(Path.home()/'.local/bin/fals3y'))
        s3log = (tmp/'s3.log').open('wb')
        storage = subprocess.Popen([fals3y, 'start', '-p', str(s3_port), '-d', str(tmp/'s3-data')], stdout=s3log, stderr=s3log)
        s3log.close()
        def s3_request(key, method='GET', data=None):
            with urllib.request.urlopen(urllib.request.Request(endpoint+key, method=method, data=data), timeout=10) as response:
                return response.read()
        try:
            for _ in range(100):
                if storage.poll() is not None:
                    raise RuntimeError((tmp/'s3.log').read_text())
                try:
                    s3_request('/')
                    break
                except OSError:
                    time.sleep(.05)
            else:
                raise RuntimeError('S3 startup timeout')
            bucket = 'email-test'
            s3_request('/'+bucket, 'PUT', b'')
            subprocess.run(['go', 'test', '-race', './...'], cwd=root, check=True,
                           env={**os.environ, 'TEST_S3_ENDPOINT':endpoint, 'TEST_S3_BUCKET':bucket})
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                            '-keyout', str(tmp/'key.pem'), '-out', str(tmp/'cert.pem'), '-subj', '/CN=localhost',
                            '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1'], check=True, capture_output=True)
            context = ssl.create_default_context(cafile=str(tmp/'cert.pem'))
            for key in ['cert.pem', 'key.pem']:
                s3_request(f'/{bucket}/{key}', 'PUT', (tmp/key).read_bytes())
                (tmp/key).unlink()
            env = {**os.environ, 'FMA_S3_ENDPOINT':endpoint, 'FMA_S3_BUCKET':bucket,
                   'FMA_S3_ACCESS_KEY_ID':'test', 'FMA_S3_SECRET_ACCESS_KEY':'test', 'FMA_OUTBOUND_MODE':'disabled'}
            work = tmp/'empty-workdir'
            work.mkdir(mode=0o500)
            for user,password in [('jw238','123123'),('jw238x','different-password')] + [(u, 'mime-password') for u in ['mime-to', 'mime-cc', 'mime-bcc', 'mime-header-only']]:
                s3_request(f'/{bucket}/{user}/.password', 'PUT', password.encode())
                s3_request(f'/{bucket}/{user}/.kind', 'PUT', b'account')
            sockets = [socket.socket() for _ in range(8)]
            for sock in sockets:
                sock.bind(('127.0.0.1', 0))
            ports = [sock.getsockname()[1] for sock in sockets]
            for sock in sockets:
                sock.close()
            args = [str(binary)]
            for name,port in zip(['smtp','submission','smtps','pop3s','imaps','http','pop3','imap'], ports):
                args.extend(['-'+name, '127.0.0.1:'+str(port)])
            logfile = tmp/'server.log'
            def start():
                with logfile.open('ab') as out:
                    proc = subprocess.Popen(args, env=env, cwd=work, stdout=out, stderr=out)
                for _ in range(200):
                    if proc.poll() is not None:
                        raise RuntimeError(logfile.read_text())
                    try:
                        with socket.create_connection(('127.0.0.1', ports[5]), timeout=.1):
                            return proc
                    except OSError:
                        time.sleep(.05)
                proc.kill()
                proc.wait()
                raise RuntimeError('server startup timeout')
            proc = start()
            try:
                verify('127.0.0.1', ports, context, isolated=True)
                verify_folders('127.0.0.1', ports[4], context)
                mime = attachment_message()
                # Freeze boundaries, then let the submitting client remove Bcc from DATA.
                mime.as_bytes()
                with smtplib.SMTP_SSL('127.0.0.1', ports[2], context=context) as sender:
                    sender.login('jw238', '123123')
                    sender.send_message(mime, to_addrs=['mime-to@t12e.cc', 'mime-cc@t12e.cc', 'mime-bcc@t12e.cc', 'mime-cc@t12e.cc'])
                del mime['Bcc']
                expected_mime = mime.as_bytes()
                verify_attachment_mailboxes('127.0.0.1', ports, context, expected_mime)
                jmap_saved = verify_jmap('127.0.0.1', ports, context, lambda key, data: s3_request(f'/{bucket}/{key}', 'PUT', data))
                proc.send_signal(signal.SIGTERM)
                require(proc.wait(timeout=15) == 0, 'unclean shutdown')
                proc = start()
                c = imaplib.IMAP4_SSL('127.0.0.1', ports[4], ssl_context=context)
                c.login('jw238','123123')
                c.select('INBOX',readonly=True)
                ids = c.uid('search',None,'ALL')[1][0].split()
                require(len(ids)==12 and min(map(int,ids))>=4, 'restart lost mail or reused UID')
                c.logout()
                verify_attachment_mailboxes('127.0.0.1', ports, context, expected_mime)
                verify_jmap_restart('127.0.0.1', ports, jmap_saved)
                require(list(work.iterdir())==[], 'mail process wrote local files')
                s3_request(f'/{bucket}/jw238/.password')
                s3_request(f'/{bucket}/jw238/.jmap/state.json')
                print('PASS S3-only storage / users and certificates in bucket / external credentials / restart / no local files')
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    proc.wait(timeout=15)
                require('DATA RACE' not in logfile.read_text(), logfile.read_text())
        finally:
            if storage.poll() is None:
                storage.terminate()
                storage.wait(timeout=15)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host')
    parser.add_argument('--connect-address', help='override connection address, retaining TLS hostname verification')
    parser.add_argument('--skip-inbound', action='store_true', help='explicitly skip blocked public port 25')
    args = parser.parse_args()
    if args.host:
        if args.connect_address:
            resolve = socket.getaddrinfo
            socket.getaddrinfo = lambda host, *a, **kw: resolve(args.connect_address if host == args.host else host, *a, **kw)
            print(f'Connection override: {args.host} -> {args.connect_address}; this is not an external reachability test')
        verify(args.host, [25, 587, 465, 995, 993, 443, 110, 143], ssl.create_default_context(), skip_inbound=args.skip_inbound)
    else:
        local()
