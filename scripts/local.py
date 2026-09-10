#!/usr/bin/env python3
"""Run native Fals3y and the mail server against one local S3 bucket. No Docker."""
import argparse
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', type=int, default=9000)
    parser.add_argument('--bucket', default='fma')
    parser.add_argument('--data', type=Path, default=Path.home()/'.local/share/fals3y/data')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    binary = root/'fma'
    if not binary.exists():
        subprocess.run(['make', 'build'], cwd=root, check=True)
    endpoint = f'http://127.0.0.1:{args.port}'
    env = {**os.environ, 'FMA_S3_ENDPOINT':endpoint, 'FMA_S3_BUCKET':args.bucket,
           'FMA_S3_REGION':'us-east-1', 'FMA_S3_ACCESS_KEY_ID':'local', 'FMA_S3_SECRET_ACCESS_KEY':'local',
           'FMA_S3_SESSION_TOKEN':'', 'FMA_OUTBOUND_MODE':os.environ.get('FMA_OUTBOUND_MODE','disabled')}
    fals3y = os.environ.get('FALS3Y_BIN', str(Path.home()/'.local/bin/fals3y'))
    storage = subprocess.Popen([fals3y, 'start', '-p', str(args.port), '-d', str(args.data)])
    mail = None

    def request(key, method='GET', data=None):
        with urllib.request.urlopen(urllib.request.Request(endpoint+key, method=method, data=data), timeout=10) as response:
            return response.read()

    def exists(key):
        try:
            request(key, 'HEAD')
            return True
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return False
            raise

    def interrupted(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    try:
        for _ in range(100):
            if storage.poll() is not None:
                raise RuntimeError('Fals3y exited; check whether its port is already in use')
            try:
                request('/')
                time.sleep(.1)
                if storage.poll() is not None:
                    raise RuntimeError('Fals3y exited; refusing to use another server on the same port')
                break
            except OSError:
                time.sleep(.05)
        else:
            raise RuntimeError('Fals3y startup timed out')
        bucket_path = '/'+args.bucket
        if not exists(bucket_path):
            request(bucket_path, 'PUT', b'')
        cert_exists = exists(bucket_path+'/cert.pem')
        key_exists = exists(bucket_path+'/key.pem')
        if cert_exists != key_exists:
            raise RuntimeError('bucket has an incomplete TLS certificate pair; existing objects were preserved')
        if not cert_exists:
            # Provisioning only: these temporary files belong to the setup script,
            # are uploaded, then removed before the mail process starts.
            with tempfile.TemporaryDirectory(prefix='fma-tls-') as tmp:
                subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '365',
                                '-keyout', tmp+'/key.pem', '-out', tmp+'/cert.pem', '-subj', '/CN=localhost',
                                '-addext', 'subjectAltName=DNS:localhost,DNS:mail.t12e.cc,IP:127.0.0.1'],
                               check=True, capture_output=True)
                for key in ['cert.pem','key.pem']:
                    request(bucket_path+'/'+key, 'PUT', (Path(tmp)/key).read_bytes())
        print(f'S3 bucket: {endpoint}/{args.bucket}', flush=True)
        print('Starting mail server with all persistent data in S3. Ctrl+C stops both services.', flush=True)
        mail = subprocess.Popen([str(binary)], env=env)
        while True:
            code = mail.poll()
            if code is not None:
                raise RuntimeError(f'Mail server exited: {code}')
            if storage.poll() is not None:
                raise RuntimeError('Fals3y exited while mail server was running')
            time.sleep(.5)
    except KeyboardInterrupt:
        pass
    finally:
        # The bucket remains available until mail shutdown has released its lock.
        for proc in [mail,storage]:
            if proc is not None and proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=120)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()


if __name__ == '__main__':
    main()
