#!/usr/bin/env python3
"""Measure real S3/JMAP streaming IO with bounded client memory and native Fals3y.

Default payload: 2 GiB. Reports fma's sampled peak RSS, process CPU seconds,
wall time and SHA-256 for both directions. No Docker or local fma spool.
"""
import argparse
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.request

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
        return {'elapsed_seconds': round(time.monotonic() - self.started, 3),
                'cpu_seconds': round(cpu - self.cpu_start, 3),
                'peak_rss_bytes': max(rss, self.peak), 'initial_rss_bytes': self.rss_start,
                'rss_sample_interval_seconds': .05}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--size-mib', type=int, default=2048)
    parser.add_argument('--max-rss-mib', type=int, default=256)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args()
    assert args.size_mib > 0
    size = args.size_mib << 20
    results = {'bytes': size, 'platform': os.uname().sysname + ' ' + os.uname().machine,
               'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
               'working_tree': 'measured from working tree; see git status', 'phases': {}}
    with tempfile.TemporaryDirectory(prefix='fma-streaming-') as directory:
        tmp = Path(directory)
        binary = tmp / 'fma'
        subprocess.run(['go', 'build', '-trimpath', '-o', str(binary), '.'], cwd=ROOT, check=True)
        s3port, port = free_port(), free_port()
        endpoint = f'http://127.0.0.1:{s3port}'
        def s3request(key, method='GET', data=None):
            with urllib.request.urlopen(urllib.request.Request(endpoint + key, method=method, data=data), timeout=30) as response:
                return response.read()
        fals3y = os.environ.get('FALS3Y_BIN', str(Path.home() / '.local/bin/fals3y'))
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
                   'FMA_OUTBOUND_MODE': 'disabled', 'LOG_LEVEL': 'error'}
            command = [str(binary), '-http', f'127.0.0.1:{port}']
            for protocol in ['smtp', 'submission', 'smtps', 'pop3', 'pop3s', 'imap', 'imaps']:
                command += ['-' + protocol, '127.0.0.1:0']
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
            for name, value in {**auth, 'Content-Type': 'application/octet-stream', 'Content-Length': str(size)}.items():
                conn.putheader(name, value)
            conn.endheaders()
            sent = 0
            while sent < size:
                chunk = BLOCK[:min(len(BLOCK), size - sent)]
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
            conn.close()
            assert list(work.iterdir()) == [], 'fma wrote local temporary data'
            assert max(phase['peak_rss_bytes'] for phase in results['phases'].values()) <= args.max_rss_mib << 20, results
            # An interrupted upload must not leave a completed blob or multipart staging data.
            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=30)
            conn.putrequest('POST', f'/upload/{account}/')
            for name, value in {**auth, 'Content-Type': 'application/octet-stream', 'Content-Length': str(size)}.items():
                conn.putheader(name, value)
            conn.endheaders(); conn.send(BLOCK * 9); conn.close()
            time.sleep(1)
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
