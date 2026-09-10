#!/usr/bin/env python3
"""JMAP integration tests against native Fals3y; also run by make test.

Run python3 scripts/verify_jmap.py for the isolated protocol suite. The fixture
owns its accounts and bucket; no existing mailbox is modified.
"""
import base64
import imaplib
import json
import poplib
import smtplib
import time
import urllib.error
import urllib.parse
import urllib.request
from email.message import EmailMessage
from email.parser import BytesParser
from email import policy

CORE = 'urn:ietf:params:jmap:core'
MAIL = 'urn:ietf:params:jmap:mail'
SUBMIT = 'urn:ietf:params:jmap:submission'


class Client:
    def __init__(self, base, user='jmap-alice', password='jmap-password'):
        self.base, self.user, self.password = base, user, password
        self.session = self.request('/.well-known/jmap')[1]
        self.account = self.session['primaryAccounts'][MAIL]

    def request(self, path, data=None, content_type='application/json', expected=200, auth=True):
        headers = {'Content-Type': content_type}
        if auth:
            headers['Authorization'] = 'Basic ' + base64.b64encode(f'{self.user}:{self.password}'.encode()).decode()
        req = urllib.request.Request(self.base + path, data=data, headers=headers)
        try:
            response = urllib.request.urlopen(req, timeout=45)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            body = response.read()
            assert response.status == expected, (path, response.status, body[:1500])
            if 'json' in response.headers.get('Content-Type', ''):
                body = json.loads(body)
            return response.status, body

    def calls(self, calls, using=None):
        payload = {'using': using or [CORE, MAIL, SUBMIT], 'methodCalls': calls}
        return self.request('/api', json.dumps(payload).encode())[1]['methodResponses']

    def call(self, method, args=None, error=None):
        args = {'accountId': self.account, **(args or {})}
        responses = self.calls([[method, args, 'test']])
        name, result, call_id = responses[0]
        assert call_id == 'test', responses
        if error:
            assert name == 'error' and result['type'] == error, responses
        else:
            assert name == method, responses
            for field in ('notCreated', 'notUpdated', 'notDestroyed'):
                assert not result.get(field), responses
        return result

    def upload(self, content, mime='application/octet-stream'):
        return self.request(f'/upload/{self.account}/', content, mime, expected=200)[1]

    def download(self, blob, name='attachment.bin', expected=200):
        return self.request(f'/download/{self.account}/{urllib.parse.quote(blob, safe="")}/{urllib.parse.quote(name, safe="")}', expected=expected)[1]


def verify_jmap(host, ports, tls, provision):
    users = ['jmap-alice', 'jmap-bob', 'jmap-cc', 'jmap-bcc', 'jmap-header-only']
    for user in users:
        provision(user + '/.password', b'jmap-password')
        provision(user + '/.kind', b'account')
    provision('jmap-alias/.alias', b'jmap-alice')
    provision('jmap-alias/.kind', b'alias')
    provision('jmap-proxy/.proxy', b'jmap-bob@t12e.cc')
    provision('jmap-proxy/.kind', b'proxy')
    base = f'http://{host}:{ports[5]}'
    a, b = Client(base), Client(base, 'jmap-bob')
    assert Client(base, 'jmap-alias').account == a.account
    a.request('/.well-known/jmap', expected=401, auth=False)
    a.password = 'wrong'
    a.request('/.well-known/jmap', expected=401)
    a.password = 'jmap-password'
    original = a.user
    a.user = 'jmap-proxy'
    a.request('/.well-known/jmap', expected=401)
    a.user = original
    assert MAIL in a.session['capabilities'] and SUBMIT in a.session['capabilities']
    assert a.account in a.session['accounts'] and b.account not in a.session['accounts']
    a.call('Mailbox/get', {'accountId': b.account}, error='accountNotFound')
    a.request('/api', b'{broken', expected=400)
    assert a.calls([['Unknown/get', {}, 'unknown']])[0][1]['type'] == 'unknownMethod'
    print('PASS JMAP discovery / credentials / aliases / proxy rejection / account isolation / malformed requests')

    boxes = a.call('Mailbox/get')
    roles = {box['role']: box['id'] for box in boxes['list'] if box['role']}
    assert set(roles) >= {'inbox', 'sent', 'drafts', 'trash', 'junk', 'archive'}, roles
    state = boxes['state']
    created = a.call('Mailbox/set', {'create': {'parent': {'name': 'JMAP tests'}, 'child': {'name': '附件', 'parentId': '#parent'}}})['created']
    folder, child = created['parent']['id'], created['child']['id']
    changes = a.call('Mailbox/changes', {'sinceState': state})
    assert {folder, child} <= set(changes['created']), changes
    a.call('Mailbox/set', {'ifInState': state, 'update': {folder: {'name': 'stale'}}}, error='stateMismatch')
    a.call('Mailbox/set', {'update': {folder: {'name': 'Renamed', 'isSubscribed': True}}})
    assert a.call('Mailbox/query', {'filter': {'parentId': folder}})['ids'] == [child]
    print('PASS JMAP mailbox hierarchy / rename / subscriptions / changes / stale-state rejection')

    attachment = bytes(range(256)) * 4096
    mime = EmailMessage(policy=policy.SMTP)
    mime['From'] = 'jmap-alice@t12e.cc'
    mime['To'] = 'jmap-bob@t12e.cc'
    mime['Subject'] = 'JMAP 附件搜索'
    mime['Message-ID'] = '<jmap-attachments@t12e.cc>'
    mime.set_content('Searchable 中文正文 needle')
    mime.add_attachment(attachment, maintype='application', subtype='octet-stream', filename='文件.bin')
    mime.add_attachment(b'', maintype='application', subtype='octet-stream', filename='empty.bin')
    raw = mime.as_bytes()
    uploaded = a.upload(raw, 'message/rfc822')
    assert uploaded['size'] == len(raw) and a.download(uploaded['blobId']) == raw
    b.download(uploaded['blobId'], expected=404)
    before = a.call('Email/get', {'ids': []})['state']
    result = a.call('Email/import', {'emails': {'message': {'blobId': uploaded['blobId'], 'mailboxIds': {folder: True}, 'keywords': {}}}})
    email_id = result['created']['message']['id']
    email = a.call('Email/get', {'ids': [email_id], 'fetchAllBodyValues': True})['list'][0]
    assert email['hasAttachment'] and len(email['attachments']) == 2, email
    for part, expected in zip(email['attachments'], (attachment, b'')):
        assert a.download(part['blobId'], part['name']) == expected, part
    assert 'needle' in str(email['bodyValues']), email
    assert email_id in a.call('Email/query', {'filter': {'text': 'needle'}})['ids']
    assert email_id in a.call('Email/query', {'filter': {'hasAttachment': True, 'inMailbox': folder}, 'sort': [{'property': 'receivedAt', 'isAscending': False}]})['ids']
    thread = a.call('Thread/get', {'ids': [email['threadId']]})['list'][0]
    assert email_id in thread['emailIds']
    snippets = a.call('SearchSnippet/get', {'emailIds': [email_id], 'filter': {'text': 'needle'}})
    assert snippets['list'][0]['emailId'] == email_id
    assert email_id in a.call('Email/changes', {'sinceState': before})['created']
    query = a.call('Email/query', {'filter': {'inMailbox': folder}, 'limit': 1, 'calculateTotal': True})
    assert query['ids'] == [email_id] and query['total'] == 1
    a.call('Email/set', {'update': {email_id: {'keywords/$seen': True, f'mailboxIds/{roles["inbox"]}': True}}})
    a.call('Email/queryChanges', {'filter': {'inMailbox': folder}, 'sinceQueryState': query['queryState']})
    print('PASS JMAP MIME import / binary and empty attachments / exact downloads / search / snippets / threads / pagination / state sync')

    with imaplib.IMAP4_SSL(host, ports[4], ssl_context=tls, timeout=45) as im:
        im.login('jmap-alice', 'jmap-password')
        assert im.select('INBOX')[0] == 'OK'
        uid = im.uid('search', None, 'HEADER', 'Message-ID', '<jmap-attachments@t12e.cc>')[1][0]
        assert uid
        fetched = im.uid('fetch', uid, '(FLAGS BODY.PEEK[])')[1]
        assert any(isinstance(v, tuple) and v[1] == raw and b'\\Seen' in v[0] for v in fetched), fetched
        assert im.uid('store', uid, '+FLAGS', '(\\Flagged)')[0] == 'OK'
    assert a.call('Email/get', {'ids': [email_id]})['list'][0]['keywords']['$flagged']
    pop = poplib.POP3_SSL(host, ports[3], context=tls, timeout=45)
    try:
        pop.user('jmap-alice'); pop.pass_('jmap-password')
        assert pop.stat()[0] == 1
        assert b'\r\n'.join(pop.retr(1)[1]) + b'\r\n' == raw
    finally:
        pop.quit()
    with smtplib.SMTP_SSL(host, ports[2], context=tls, timeout=45) as smtp:
        smtp.login('jmap-bob', 'jmap-password')
        smtp.sendmail('jmap-bob@t12e.cc', ['jmap-alice@t12e.cc'], b'From: jmap-bob@t12e.cc\r\nTo: jmap-alice@t12e.cc\r\nSubject: SMTP to JMAP\r\n\r\ninterop\r\n')
    assert a.call('Email/query', {'filter': {'subject': 'SMTP to JMAP'}})['ids']
    print('PASS shared JMAP / SMTP / IMAP / POP3 storage and flags')

    # Compose using an uploaded attachment; the server materializes the MIME.
    blob = a.upload(attachment)
    identities = a.call('Identity/get')['list']
    assert identities
    draft = {'mailboxIds': {roles['drafts']: True}, 'keywords': {'$draft': True},
             'from': [{'email': 'jmap-alice@t12e.cc'}], 'to': [{'email': 'jmap-bob@t12e.cc'}],
             'cc': [{'email': 'jmap-cc@t12e.cc'}], 'bcc': [{'email': 'jmap-bcc@t12e.cc'}],
             'subject': 'JMAP composed message', 'textBody': [{'partId': 'text', 'type': 'text/plain'}],
             'bodyValues': {'text': {'value': 'Created with JMAP'}},
             'attachments': [{'blobId': blob['blobId'], 'type': 'application/octet-stream', 'name': '二进制.bin', 'disposition': 'attachment'}]}
    composed = a.call('Email/set', {'create': {'draft': draft}})['created']['draft']['id']
    submitted = a.call('EmailSubmission/set', {'create': {'send': {'emailId': composed, 'identityId': identities[0]['id']}},
                                             'onSuccessUpdateEmail': {'#send': {f'mailboxIds/{roles["drafts"]}': None, f'mailboxIds/{roles["sent"]}': True, 'keywords/$draft': None}}})
    submission = submitted['created']['send']['id']
    # Durable receipt and message must survive a process restart; verification
    # continues after the fixture restarts fma against exactly the same bucket.
    print('PASS JMAP attachment composition / identity / durable EmailSubmission')
    return {'email': email_id, 'submission': submission, 'composed': composed, 'attachment': attachment,
            'state': a.call('Email/get', {'ids': []})['state'], 'folder': folder, 'child': child}


def verify_jmap_restart(host, ports, saved):
    base = f'http://{host}:{ports[5]}'
    a = Client(base)
    assert a.call('Email/get', {'ids': [saved['email']]})['list']
    assert not a.call('Email/changes', {'sinceState': saved['state']})['destroyed']
    deadline = time.monotonic() + 75
    while True:
        receipt = a.call('EmailSubmission/get', {'ids': [saved['submission']]})['list'][0]
        if receipt['undoStatus'] == 'final':
            break
        assert time.monotonic() < deadline, receipt
        time.sleep(.5)
    assert all(value['delivered'] in ('yes', 'unknown') for value in receipt['deliveryStatus'].values()), receipt
    for user in ['jmap-bob', 'jmap-cc', 'jmap-bcc']:
        c = Client(base, user)
        ids = c.call('Email/query', {'filter': {'subject': 'JMAP composed message'}})['ids']
        assert len(ids) == 1, (user, ids)
        message = c.call('Email/get', {'ids': ids})['list'][0]
        raw = c.download(message['blobId'])
        parsed = BytesParser(policy=policy.default).parsebytes(raw)
        assert parsed['Bcc'] is None
        assert list(parsed.iter_attachments())[0].get_payload(decode=True) == saved['attachment']
    untouched = Client(base, 'jmap-header-only')
    assert not untouched.call('Email/query')['ids']
    a.call('Email/set', {'destroy': [saved['email']]})
    assert saved['email'] in a.call('Email/changes', {'sinceState': saved['state']})['destroyed']
    a.call('Mailbox/set', {'destroy': [saved['child'], saved['folder']], 'onDestroyRemoveEmails': True})
    print('PASS JMAP restart / queued sending / To-Cc-Bcc / attachment delivery / destruction sync')


if __name__ == '__main__':
    from verify import local
    local()
