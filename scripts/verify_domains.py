"""Multi-domain protocol isolation checks, run by verify.py against native S3."""
import base64
from email.message import EmailMessage
from email.policy import SMTP
import imaplib
import json
import poplib
import smtplib
import urllib.error
import urllib.request


def verify_domains(host, ports, context, saved=None):
    domains = ('example.com', 'example.org')
    passwords = ('first-domain-password', 'second-domain-password')

    def http(user, password, path, data=None, content_type='application/json'):
        headers = {'Authorization': 'Basic ' + base64.b64encode(f'{user}:{password}'.encode()).decode(),
                   'Content-Type': content_type}
        request = urllib.request.Request(f'http://{host}:{ports[5]}{path}', data=data, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=20) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    if saved is None:
        saved = {domain: [] for domain in domains}
        for domain in domains:
            message = EmailMessage(policy=SMTP)
            message['From'] = 'sender@outside.example'
            message['To'] = f'alice@{domain}'
            message['Subject'] = f'private {domain}'
            message.set_content(f'Only {domain} should receive this.')
            raw = message.as_bytes()
            with smtplib.SMTP(host, ports[0], timeout=20) as client:
                client.sendmail('sender@outside.example', [f'alice@{domain}'], raw)
                assert client.mail('sender@outside.example')[0] == 250
                assert client.rcpt('missing@example.org')[0] == 550
            saved[domain].append(raw)
        message = EmailMessage(policy=SMTP)
        message['From'] = 'alice@example.com'
        message['To'] = 'alice@example.org'
        message['Subject'] = 'explicit cross-domain delivery'
        message.set_content('An intentional delivery between hosted domains.')
        message.add_attachment(bytes(range(256)) * 2048, maintype='application', subtype='octet-stream', filename='domain.bin')
        raw = message.as_bytes()
        with smtplib.SMTP_SSL(host, ports[2], context=context, timeout=20) as client:
            client.login('alice@example.com', passwords[0])
            assert client.mail('alice@example.org')[0] == 553, 'cross-domain sender spoofing'
            client.rset()
            client.sendmail('alice@example.com', ['alice@example.org'], raw)
        saved['example.org'].append(raw)

    sessions = {}
    for index, domain in enumerate(domains):
        address = f'alice@{domain}'
        with imaplib.IMAP4_SSL(host, ports[4], ssl_context=context, timeout=20) as client:
            try:
                client.login(address, passwords[1-index])
            except imaplib.IMAP4.error:
                pass
            else:
                raise AssertionError('IMAP password crossed domains')
            client.login(address, passwords[index])
            assert client.select('INBOX', readonly=True)[0] == 'OK'
            ids = client.uid('search', None, 'ALL')[1][0].split()
            assert len(ids) == len(saved[domain]), ('IMAP mailbox isolation', domain, ids)
            for uid, expected in zip(ids, saved[domain]):
                result = client.uid('fetch', uid, '(BODY.PEEK[])')[1]
                actual = next(item[1] for item in result if isinstance(item, tuple))
                assert actual == expected, ('IMAP body mismatch', domain)
        client = poplib.POP3_SSL(host, ports[3], context=context, timeout=20)
        try:
            client.user(address)
            client.pass_(passwords[index])
            assert client.stat()[0] == len(saved[domain]), 'POP3 mailbox isolation'
            actual = b'\r\n'.join(client.retr(1)[1]) + b'\r\n'
            assert actual == saved[domain][0], 'POP3 body mismatch'
        finally:
            client.quit()
        status, body = http(address, passwords[index], '/.well-known/jmap')
        assert status == 200, body
        session = json.loads(body)
        account = session['primaryAccounts']['urn:ietf:params:jmap:mail']
        assert session['accounts'][account]['name'] == address
        assert session['apiUrl'] == f'https://mail.{domain}/api'
        sessions[domain] = account
        status, body = http(address, passwords[1-index], '/.well-known/jmap')
        assert status == 401, 'JMAP password crossed domains'
        request = {'using': ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
                   'methodCalls': [['Email/query', {'accountId': account}, 'q']]}
        status, body = http(address, passwords[index], '/api', json.dumps(request).encode())
        result = json.loads(body)['methodResponses'][0]
        # The sender also owns a Sent copy of the intentional delivery.
        assert result[0] == 'Email/query' and len(result[1]['ids']) == 2, body

    assert sessions[domains[0]] != sessions[domains[1]], 'JMAP account IDs collided'
    status, body = http('alice@example.com', passwords[0], '/upload/' + sessions[domains[1]], b'forbidden', 'application/octet-stream')
    assert status >= 400, ('cross-domain JMAP upload accepted', body)
    for address in ('example.org/alice', 'alice@unknown.example'):
        status, _ = http(address, passwords[1], '/.well-known/jmap')
        assert status == 401, 'path or unknown domain accepted as login'
    print('PASS multi-domain SMTP / IMAP / POP3 / JMAP isolation and cross-domain delivery')
    return saved
