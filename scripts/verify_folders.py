#!/usr/bin/env python3
"""Exercise writable IMAP folders using disposable mailboxes, preserving real mail."""
import imaplib
import ssl
import uuid


def verify_folders(host, port, context):
    name = 'fma-test-' + uuid.uuid4().hex
    renamed = name + '-renamed'
    target = name + '-copy'
    body = (f'From: jw238@t12e.cc\r\nTo: jw238@t12e.cc\r\nSubject: {name}\r\n'
            f'Message-ID: <{name}@t12e.cc>\r\nMIME-Version: 1.0\r\n'
            'Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n中文显示验证\r\n').encode()
    c = imaplib.IMAP4_SSL(host,port,ssl_context=context,timeout=15)
    c.login('jw238','123123')
    assert 'IDLE' not in c.capability()[1][0].decode().split()
    assert 'MOVE' not in c.capability()[1][0].decode().split()
    created=[]
    try:
        kind, folders = c.list()
        assert kind == 'OK'
        for expected in (b'INBOX', b'Sent', b'Drafts', b'Trash', b'Junk', b'Archive'):
            assert any(expected in row for row in folders), (expected,folders)
        assert any(b'\\Sent' in row and b'Sent' in row for row in folders), folders
        for folder in (name,target):
            assert c.create(folder)[0]=='OK'
            created.append(folder)
        assert c.append(name,None,None,body)[0]=='OK'
        assert c.select(name)[1]==[b'1']
        assert c.response('READ-WRITE')[0]=='READ-WRITE'
        assert c.search(None,'UNSEEN')[1]==[b'1']
        result=c.fetch('1','(UID FLAGS ENVELOPE BODYSTRUCTURE BODY.PEEK[])')
        assert result[0]=='OK' and any(isinstance(x,tuple) and x[1]==body for x in result[1]),result
        assert c.search(None,'UNSEEN')[1]==[b'1'], 'PEEK unexpectedly set Seen'
        assert c.fetch('1','(BODY[])')[0]=='OK'
        assert c.search(None,'SEEN')[1]==[b'1'], 'body read did not set Seen'
        assert c.store('1','+FLAGS',r'(\Flagged)')[0]=='OK'
        assert c.copy('1',target)[0]=='OK'
        assert c.unsubscribe(target)[0]=='OK'
        assert not any(target.encode() in row for row in c.lsub()[1] if row), 'LSUB ignored unsubscribe'
        assert c.subscribe(target)[0]=='OK'
        # APPEND from another connection must become visible after NOOP.
        other=imaplib.IMAP4_SSL(host,port,ssl_context=context,timeout=15)
        other.login('jw238','123123')
        try:
            assert other.append(name,None,None,body.replace(name.encode(),b'second-message'))[0]=='OK'
        finally:
            other.logout()
        assert c.noop()[0]=='OK'
        assert c.search(None,'ALL')[1]==[b'1 2'], 'NOOP did not refresh selected mailbox'
        assert c.store('1','+FLAGS',r'(\Deleted)')[0]=='OK'
        assert c.expunge()[0]=='OK'
        assert c.select(name)[1]==[b'1']
        c.close()
        assert c.rename(name,renamed)[0]=='OK'
        created.remove(name); created.append(renamed)
        assert c.select(target)[1]==[b'1']
        assert c.search(None,'FLAGGED')[1]==[b'1'], 'COPY lost flags'
        c.close()
        c.logout()
        c=imaplib.IMAP4_SSL(host,port,ssl_context=context,timeout=15)
        c.login('jw238','123123')
        assert c.select(target)[1]==[b'1']
        assert c.search(None,'FLAGGED')[1]==[b'1'], 'flags lost across sessions'
        assert c.fetch('1','(BODY.PEEK[])')[0]=='OK'
        c.close()
        print('PASS IMAP folders / Sent discovery / APPEND / MIME FETCH / Seen / STORE / COPY / EXPUNGE / NOOP / subscriptions / rename')
    finally:
        for folder in created:
            c.delete(folder)
        c.logout()


if __name__=='__main__':
    verify_folders('mail.t12e.cc',993,ssl.create_default_context())
