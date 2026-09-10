#!/usr/bin/env python3
"""Measure real S3/JMAP streaming IO with bounded client memory and native Fals3y.

Default payload: 2 GiB. Reports fma's sampled peak RSS, process CPU seconds,
wall time and SHA-256 for both directions. No Docker or local fma spool.
"""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
import http.client
import json
import os
import random
import re
from pathlib import Path
import signal
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
BLOCK = bytes(range(256)) * 4096


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def process_usage(pid):
    output = subprocess.check_output(['ps', '-p', str(pid), '-o', 'rss=', '-o', 'time='], text=True).split()
    cpu = output[1]
    days = 0
    if '-' in cpu:
        day, cpu = cpu.split('-', 1)
        days = int(day)
    seconds = sum(float(part) * 60**i for i, part in enumerate(reversed(cpu.split(':'))))
    return int(output[0]) * 1024, seconds + days * 86400


class Measurement:
    def __init__(self, pid):
        self.pid = pid
        self.peak, self.cpu_start = process_usage(pid)
        self.rss_start = self.peak
        self.started_utc = datetime.now(timezone.utc).isoformat()
        self.started = time.monotonic()
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.sample, daemon=True)
        self.thread.start()

    def sample(self):
        while not self.stop.wait(.05):
            try:
                self.peak = max(self.peak, process_usage(self.pid)[0])
            except (OSError, subprocess.SubprocessError, IndexError):
                return

    def finish(self):
        self.stop.set(); self.thread.join()
        rss, cpu = process_usage(self.pid)
        elapsed = time.monotonic() - self.started
        cpu_used = cpu - self.cpu_start
        peak = max(rss, self.peak)
        return {'started_utc': self.started_utc,
                'finished_utc': datetime.now(timezone.utc).isoformat(),
                'elapsed_seconds': round(elapsed, 3),
                'cpu_seconds': round(cpu_used, 3),
                'average_cpu_percent_one_core': round(100 * cpu_used / elapsed, 2),
                'peak_rss_bytes': peak, 'initial_rss_bytes': self.rss_start,
                'peak_rss_growth_bytes': max(0, peak - self.rss_start),
                'rss_sample_interval_seconds': .05}


def mail_benchmark(proc, ports, conn, auth, account, size, block, results, smtp_transfer, repeat_jmap=False, jmap_first=False):
    """Real MIME attachment IO, with no full-message client buffers."""
    import imaplib
    import smtplib
    import poplib
    from contextlib import closing
    boundary = 'fma-streaming-benchmark-boundary'
    prefix = (f'From: sender@example.net\r\nTo: alice@t12e.cc\r\nSubject: streaming attachment benchmark\r\n'
              f'Message-ID: <streaming-benchmark@example.net>\r\nMIME-Version: 1.0\r\n'
              f'Content-Type: multipart/mixed; boundary="{boundary}"\r\n\r\n'
              f'--{boundary}\r\nContent-Type: text/plain\r\n\r\nStreaming attachment.\r\n'
              f'--{boundary}\r\nContent-Type: application/octet-stream\r\n'
              'Content-Disposition: attachment; filename="large.bin"\r\n'
              'Content-Transfer-Encoding: base64\r\n\r\n').encode()
    suffix = f'\r\n--{boundary}--\r\n'.encode()
    block = block[:len(block) // 57 * 57]
    encoded = base64.encodebytes(block).replace(b'\n', b'\r\n')
    def chunks():
        yield prefix
        left = size
        while left:
            n = min(left, len(block))
            yield encoded if n == len(block) else base64.encodebytes(block[:n]).replace(b'\n', b'\r\n')
            left -= n
        yield suffix
    raw_hash, attachment_hash, raw_size = hashlib.sha256(), hashlib.sha256(), 0
    for chunk in chunks():
        raw_hash.update(chunk); raw_size += len(chunk)
    left = size
    while left:
        n = min(left, len(block)); attachment_hash.update(block[:n]); left -= n
    results['mail'] = {'attachment_bytes': size, 'mime_bytes': raw_size,
                       'mime_sha256': raw_hash.hexdigest(), 'attachment_sha256': attachment_hash.hexdigest()}
    def phase(name, operation):
        print('Measuring ' + name, flush=True)
        measure = Measurement(proc.pid)
        result = operation()
        results['phases'][name] = measure.finish()
        print(json.dumps({name: results['phases'][name]}), flush=True)
        return result
    def copy_exact(reader, length, expected):
        digest, count = hashlib.sha256(), 0
        while count < length:
            chunk = reader.read(min(1 << 20, length-count))
            assert chunk, (count, length)
            digest.update(chunk); count += len(chunk)
        assert digest.hexdigest() == expected, (digest.hexdigest(), expected)
    def smtp_upload():
        with smtplib.SMTP('127.0.0.1', ports['smtp'], timeout=600) as client:
            client.ehlo()
            assert client.mail('sender@example.net')[0] == 250
            assert client.rcpt('alice@t12e.cc')[0] == 250
            if smtp_transfer == 'bdat':
                assert client.has_extn('chunking'), 'server did not advertise CHUNKING'
                if raw_size > 0xffffffff:
                    raise ValueError('single-chunk BDAT benchmark exceeds the 32-bit chunk size')
                client.putcmd('BDAT', f'{raw_size} LAST')
                for chunk in chunks(): client.sock.sendall(chunk)
            else:
                assert client.docmd('DATA')[0] == 354
                for chunk in chunks(): client.sock.sendall(chunk)
                client.sock.sendall(b'.\r\n')
            reply = client.getreply()
            assert reply[0] == 250, reply
    phase('smtp_bdat_mime_upload' if smtp_transfer == 'bdat' else 'smtp_mime_upload', smtp_upload)
    def measure_jmap():
        def jmap(method, arguments):
            request = {'using': ['urn:ietf:params:jmap:core', 'urn:ietf:params:jmap:mail'],
                       'methodCalls': [[method, {'accountId': account, **arguments}, 'b']]}
            conn.request('POST', '/api', json.dumps(request), {**auth, 'Content-Type': 'application/json'})
            response = conn.getresponse(); body = json.load(response)
            assert response.status == 200, body
            invocation = body['methodResponses'][0]; assert invocation[0] == method, invocation
            return invocation[1]
        query = jmap('Email/query', {})
        def attachment_metadata():
            data = jmap('Email/get', {'ids': query['ids'][:1], 'properties': ['id', 'attachments']})
            attachment = data['list'][0]['attachments'][0]
            assert attachment['size'] == size, attachment
            return attachment['blobId']
        blob_id = phase('jmap_attachment_metadata', attachment_metadata)
        def attachment_download():
            conn.request('GET', f'/download/{account}/{blob_id}/large.bin', headers=auth)
            response = conn.getresponse(); assert response.status == 200, response.read(1000) if response.status != 200 else ''
            copy_exact(response, size, attachment_hash.hexdigest())
            assert response.read(1) == b''
        phase('jmap_attachment_download', attachment_download)
        if repeat_jmap:
            assert phase('jmap_repeat_metadata', attachment_metadata) == blob_id
            phase('jmap_repeat_download', attachment_download)
    if jmap_first:
        measure_jmap()
    context = ssl._create_unverified_context()
    with imaplib.IMAP4_SSL('127.0.0.1', ports['imaps'], ssl_context=context, timeout=600) as client:
        client.login('alice', 'password')
        assert client.select('INBOX')[0] == 'OK'
        uid = client.uid('search', None, 'ALL')[1][0].split()[0]
        def fetch():
            tag = client._new_tag()
            client.send(tag + b' UID FETCH ' + uid + b' (BODY.PEEK[])\r\n')
            header = client.readline()
            match = re.search(rb'\{(\d+)\}\r\n$', header)
            assert match and int(match[1]) == raw_size, header
            copy_exact(client, raw_size, raw_hash.hexdigest())
            assert client.readline() == b')\r\n'
            assert client._get_tagged_response(tag)[0] == 'OK'
        phase('imap_mime_download', fetch)
        def append():
            tag = client._new_tag()
            client.send(tag + b' APPEND INBOX {' + str(raw_size).encode() + b'}\r\n')
            assert client.readline().startswith(b'+')
            for chunk in chunks(): client.send(chunk)
            client.send(b'\r\n')
            assert client._get_tagged_response(tag)[0] == 'OK'
        phase('imap_mime_append', append)
    with closing(poplib.POP3_SSL('127.0.0.1', ports['pop3s'], context=context, timeout=600)) as client:
        client.user('alice'); client.pass_('password')
        assert client.stat() == (2, 2*raw_size)
        def retr():
            client._putcmd('RETR 1'); assert client._getresp().startswith(b'+OK')
            # Generated MIME contains no dot-leading lines, so its POP wire body
            # is byte-identical. Dot stuffing is covered by verify.py.
            copy_exact(client.file, raw_size, raw_hash.hexdigest())
            assert client.file.readline() == b'.\r\n'
        phase('pop3_mime_download', retr)
        client.quit()
    if not jmap_first:
        measure_jmap()

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--size-mib', type=int, default=2048)
    parser.add_argument('--max-rss-mib', type=int, default=256)
    parser.add_argument('--report', type=Path)
    parser.add_argument('--mail', action='store_true', help='also send and receive a real MIME attachment through SMTP, IMAP, POP3 and JMAP')
    parser.add_argument('--data', choices=['compressible', 'incompressible'], default='incompressible')
    parser.add_argument('--smtp-transfer', choices=['data', 'bdat'], default='data', help='SMTP DATA or advertised CHUNKING/BDAT; reports them separately')
    parser.add_argument('--repeat-jmap', action='store_true', help='repeat metadata and attachment download to separate locator reuse from first-download work')
    parser.add_argument('--seed', type=int, help='reproducible random fixture seed for paired performance comparisons')
    parser.add_argument('--jmap-first', action='store_true', help='measure the first JMAP attachment request immediately after SMTP, before IMAP/POP3 reads')
    args = parser.parse_args()
    # Repeat a random 1 MiB block: its period exceeds gzip's 32 KiB window.
    # Generate it outside the timed phase, keeping client memory bounded.
    block = BLOCK if args.data == 'compressible' else (random.Random(args.seed).randbytes(1 << 20) if args.seed is not None else os.urandom(1 << 20))
    assert args.size_mib > 0
    size = args.size_mib << 20
    fals3y_requested = os.environ.get('FALS3Y_BIN', str(Path.home() / '.local/bin/fals3y'))
    fals3y = str(Path(shutil.which(fals3y_requested) or fals3y_requested).resolve(strict=True))
    with open(fals3y, 'rb') as executable:
        fals3y_sha256 = hashlib.file_digest(executable, 'sha256').hexdigest()
    fals3y_version = subprocess.run([fals3y, 'version'], capture_output=True, text=True, timeout=10)
    results = {'jmap_first': args.jmap_first, 'fixture_seed': args.seed, 'repeat_jmap': args.repeat_jmap, 'bytes': size, 'platform': os.uname().sysname + ' ' + os.uname().machine,
               'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
               'pop3_module': json.loads(subprocess.check_output(
                   ['go', 'list', '-m', '-json', 'github.com/Jabberwocky238/go-pop3'], cwd=ROOT, text=True)),
               'fals3y': {'requested_path': fals3y_requested, 'resolved_path': fals3y,
                          'sha256': fals3y_sha256, 'version': (fals3y_version.stdout + fals3y_version.stderr).strip(),
                          'version_exit_code': fals3y_version.returncode},
               'go_flags': os.environ.get('GOFLAGS', ''),
               'working_tree': 'measured from working tree; see git status', 'phases': {}, 'data': args.data, 'smtp_transfer': args.smtp_transfer}
    with tempfile.TemporaryDirectory(prefix='fma-streaming-') as directory:
        tmp = Path(directory)
        binary = tmp / 'fma'
        subprocess.run(['go', 'build', '-trimpath', '-o', str(binary), '.'], cwd=ROOT, check=True)
        with binary.open('rb') as executable:
            results['binary_sha256'] = hashlib.file_digest(executable, 'sha256').hexdigest()
        s3port, port = free_port(), free_port()
        endpoint = f'http://127.0.0.1:{s3port}'
        def s3request(key, method='GET', data=None):
            with urllib.request.urlopen(urllib.request.Request(endpoint + key, method=method, data=data), timeout=30) as response:
                return response.read()
        with (tmp / 's3.log').open('wb') as output:
            storage = subprocess.Popen([fals3y, 'start', '-p', str(s3port), '-d', str(tmp / 's3')], stdout=output, stderr=output)
        proc = None
        try:
            for _ in range(200):
                try:
                    s3request('/')
                    break
                except OSError:
                    if storage.poll() is not None:
                        raise RuntimeError((tmp / 's3.log').read_text())
                    time.sleep(.05)
            else:
                raise RuntimeError('Fals3y did not start')
            s3request('/stream-test', 'PUT', b'')
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                            '-keyout', str(tmp / 'key.pem'), '-out', str(tmp / 'cert.pem'),
                            '-subj', '/CN=localhost'], check=True, capture_output=True)
            for key in ['key.pem', 'cert.pem']:
                s3request('/stream-test/' + key, 'PUT', (tmp / key).read_bytes())
            for key, value in [('alice/.password', b'password'), ('alice/.kind', b'account')]:
                s3request('/stream-test/' + key, 'PUT', value)
            work = tmp / 'empty'; work.mkdir(mode=0o500)
            env = {**os.environ, 'FMA_S3_ENDPOINT': endpoint, 'FMA_S3_BUCKET': 'stream-test',
                   'FMA_S3_ACCESS_KEY_ID': 'test', 'FMA_S3_SECRET_ACCESS_KEY': 'test',
                   'FMA_OUTBOUND_MODE': 'disabled', 'LOG_LEVEL': 'debug'}
            mail_ports = {name: free_port() for name in ['smtp', 'imaps', 'pop3s']}
            command = [str(binary), '-http', f'127.0.0.1:{port}']
            for protocol in ['smtp', 'submission', 'smtps', 'pop3', 'pop3s', 'imap', 'imaps']:
                command += ['-' + protocol, f'127.0.0.1:{mail_ports.get(protocol, 0)}']
            with (tmp / 'fma.log').open('wb') as output:
                proc = subprocess.Popen(command, env=env, cwd=work, stdout=output, stderr=output)
            for _ in range(200):
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=.1):
                        break
                except OSError:
                    if proc.poll() is not None:
                        raise RuntimeError((tmp / 'fma.log').read_text())
                    time.sleep(.05)
            else:
                raise RuntimeError('fma did not start')
            auth = {'Authorization': 'Basic ' + base64.b64encode(b'alice:password').decode()}
            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=600)
            conn.request('GET', '/.well-known/jmap', headers=auth)
            response = conn.getresponse(); session = json.load(response)
            assert response.status == 200, session
            account = session['primaryAccounts']['urn:ietf:params:jmap:mail']
            print(f'Streaming {args.size_mib} MiB upload into S3; fma PID {proc.pid}', flush=True)
            measure = Measurement(proc.pid)
            digest = hashlib.sha256()
            conn.putrequest('POST', f'/upload/{account}/')
            for name, value in {**auth, 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment; filename="large.bin"', 'Subject': 'streaming benchmark', 'Content-Length': str(size)}.items():
                conn.putheader(name, value)
            conn.endheaders()
            sent = 0
            while sent < size:
                chunk = block[:min(len(block), size - sent)]
                conn.send(chunk); digest.update(chunk); sent += len(chunk)
                if sent % (256 << 20) == 0:
                    print(f'  uploaded {sent >> 20} MiB', flush=True)
            response = conn.getresponse(); body = response.read()
            results['phases']['upload'] = measure.finish()
            assert response.status == 200, (response.status, body[:1000], (tmp / 'fma.log').read_text())
            uploaded = json.loads(body)
            expected_id = 'G' + base64.urlsafe_b64encode(digest.digest()).decode().rstrip('=')
            assert uploaded['blobId'] == expected_id and uploaded['size'] == size, uploaded
            results['sha256'] = digest.hexdigest()
            object_key = f'/stream-test/alice/.jmap/blobs/{uploaded["blobId"]}'
            reference = json.loads(s3request(object_key))
            assert reference['key'].startswith('alice/mail/'), reference
            results['object_key'] = reference['key']
            metadata = reference.get('metadata', {})
            physical_url = endpoint + '/stream-test/' + urllib.parse.quote(reference['key'], safe='/')
            with urllib.request.urlopen(urllib.request.Request(physical_url, method='HEAD')) as stored:
                results['stored_bytes'] = int(stored.headers['Content-Length'])
                results['storage_encoding'] = metadata.get('fma-encoding', 'identity')
                assert (results['storage_encoding'] == 'gzip') == (size > (10 << 20)), dict(stored.headers)
                if size > (10 << 20):
                    assert int(metadata['fma-size']) == size
                    if args.data == 'compressible':
                        assert results['stored_bytes'] < size, results
                    else:
                        assert results['stored_bytes'] >= size * .99, results

            results['s3_commit_timings'] = [dict(zip(['parts', 'complete_seconds', 'index_seconds', 'copy_seconds'], map(float, values)))
                for values in re.findall(r'parts=(\d+) complete_seconds=([\d.e+-]+) index_seconds=([\d.e+-]+) copy_seconds=([\d.e+-]+)', (tmp / 'fma.log').read_text())]
            print(json.dumps({'upload': results['phases']['upload']}), flush=True)
            print('Streaming S3 through JMAP download; client hashes chunks without retaining the file', flush=True)
            measure = Measurement(proc.pid)
            conn.request('GET', f'/download/{account}/{expected_id}/large.bin', headers=auth)
            response = conn.getresponse()
            assert response.status == 200, (response.status, response.read(1000))
            received, digest = 0, hashlib.sha256()
            while chunk := response.read(1 << 20):
                received += len(chunk); digest.update(chunk)
            results['phases']['download'] = measure.finish()
            assert received == size and digest.hexdigest() == results['sha256'], (received, digest.hexdigest())
            if args.mail:
                mail_benchmark(proc, mail_ports, conn, auth, account, size, block, results, args.smtp_transfer, args.repeat_jmap, args.jmap_first)
            conn.close()
            assert list(work.iterdir()) == [], 'fma wrote local temporary data'
            assert max(phase['peak_rss_bytes'] for phase in results['phases'].values()) <= args.max_rss_mib << 20, results
            # An interrupted upload must not leave a completed blob or multipart staging data.
            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=30)
            conn.putrequest('POST', f'/upload/{account}/')
            for name, value in {**auth, 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment; filename="large.bin"', 'Subject': 'streaming benchmark', 'Content-Length': str(size)}.items():
                conn.putheader(name, value)
            conn.endheaders(); conn.send(BLOCK * 9); conn.close()
            time.sleep(1)
            for name, phase in results['phases'].items():
                if name.endswith('_metadata'):
                    continue
                transferred = results['mail']['mime_bytes'] if 'mime' in name else size
                phase['bytes'] = transferred
                phase['mib_per_second'] = round(transferred / (1 << 20) / phase['elapsed_seconds'], 2)
                phase['attachment_mib_per_second'] = round(size / (1 << 20) / phase['elapsed_seconds'], 2)
            results['mail_stream_timings'] = [line for line in (tmp / 'fma.log').read_text().splitlines()
                                              if any(marker in line for marker in ['S3 stream commit', 'mail stream stored', 'mail stream delivery'])]
            with open(fals3y, 'rb') as executable:
                assert hashlib.file_digest(executable, 'sha256').hexdigest() == fals3y_sha256, 'Fals3y executable changed during benchmark'
            results['success'] = True
            print(json.dumps(results, indent=2), flush=True)
            if args.report:
                args.report.write_text(json.dumps(results, indent=2) + '\n')
        except BaseException:
            if (tmp / 'fma.log').exists():
                print((tmp / 'fma.log').read_text(), flush=True)
            raise
        finally:
            if proc and proc.poll() is None:
                proc.send_signal(signal.SIGTERM)
                try:
                    proc.wait(timeout=35)
                except subprocess.TimeoutExpired:
                    proc.kill(); proc.wait()
            if storage.poll() is None:
                storage.terminate(); storage.wait(timeout=15)


if __name__ == '__main__':
    main()
