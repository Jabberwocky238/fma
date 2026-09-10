#!/usr/bin/env python3
"""Check LOGIN over TLS without sending mail. Target defaults to public service."""
import argparse
import base64
import smtplib
import ssl


def verify(host, implicit_port, starttls_port):
    context = ssl.create_default_context()
    for implicit in (True, False):
        for initial in (True, False):
            for user, password, expected in (
                ('jw238@t12e.cc', '123123', 235),
                ('jw238', '123123', 235),
                ('jw238', 'incorrect-password', 535),
                ('nonexistent-user', '123123', 535),
            ):
                c = (smtplib.SMTP_SSL(host, implicit_port, timeout=10, context=context)
                     if implicit else smtplib.SMTP(host, starttls_port, timeout=10))
                with c:
                    c.ehlo('fma-login-compatibility-test')
                    if not implicit:
                        assert not c.has_extn('auth')
                        assert c.docmd('AUTH', 'LOGIN')[0] == 523
                        c.starttls(context=context)
                        c.ehlo('fma-login-compatibility-test')
                    assert set(c.esmtp_features['auth'].split()) >= {'PLAIN', 'LOGIN'}
                    first = 'LOGIN'
                    if initial:
                        first += ' ' + base64.b64encode(user.encode()).decode()
                    code, challenge = c.docmd('AUTH', first)
                    if not initial:
                        assert code == 334 and base64.b64decode(challenge) == b'Username:'
                        code, challenge = c.docmd(base64.b64encode(user.encode()).decode())
                    assert code == 334 and base64.b64decode(challenge) == b'Password:'
                    code, response = c.docmd(base64.b64encode(password.encode()).decode())
                    assert code == expected, (code, response)
                    if expected != 235:
                        assert c.mail('jw238@t12e.cc')[0] == 530
                    else:
                        assert c.mail('jw238@t12e.cc')[0] == 250
                        c.rset()
    print('PASS LOGIN: implicit TLS / STARTTLS / initial response / challenges / aliases / bad credentials / plaintext rejection; no DATA sent')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--host', default='mail.t12e.cc')
    parser.add_argument('--smtps', type=int, default=465)
    parser.add_argument('--submission', type=int, default=587)
    args = parser.parse_args()
    verify(args.host, args.smtps, args.submission)
