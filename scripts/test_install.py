#!/usr/bin/env python3
"""Test installer update decisions and verified replacement with mocked downloads."""
import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='fma-install-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'installed'
        self.mock = self.root / 'mock'
        self.mock.mkdir()
        curl = self.mock / 'curl'
        curl.write_text(f'#!{sys.executable}\n' + '''import os, pathlib, shutil, sys
args = sys.argv[1:]
url = next(a for a in args if a.startswith('https://'))
root = pathlib.Path(os.environ['INSTALL_TEST_ROOT'])
with (root / 'requests').open('a') as f: f.write(url + '\\n')
if url.endswith('/releases/latest'):
    print('https://github.com/Jabberwocky238/fma/releases/tag/v1.2.0', end='')
else:
    target = args[args.index('--output') + 1]
    source = 'checksums.txt' if url.endswith('checksums.txt') else 'archive.tar.gz'
    shutil.copyfile(root / source, target)
''')
        curl.chmod(0o755)
        data = b'#!/bin/bash\nprintf "fma 1.2.0\\n"\n'
        with tarfile.open(self.root / 'archive.tar.gz', 'w:gz') as t:
            info = tarfile.TarInfo('fma')
            info.size, info.mode = len(data), 0o755
            t.addfile(info, io.BytesIO(data))
        digest = hashlib.sha256((self.root / 'archive.tar.gz').read_bytes()).hexdigest()
        system = {'Darwin': 'darwin', 'Linux': 'linux'}[os.uname().sysname]
        arch = {'x86_64': 'amd64', 'arm64': 'arm64', 'aarch64': 'arm64'}[os.uname().machine]
        (self.root / 'checksums.txt').write_text(f'{digest}  fma_1.2.0_{system}_{arch}.tar.gz\n')
        self.env = {**os.environ, 'PATH': str(self.mock) + os.pathsep + os.environ['PATH'],
                    'FMA_REPO': 'Jabberwocky238/fma', 'FMA_INSTALL_DIR': str(self.bin),
                    'INSTALL_TEST_ROOT': str(self.root)}

    def existing(self, version):
        self.bin.mkdir(exist_ok=True)
        f = self.bin / 'fma'
        f.write_text(f'#!/bin/bash\nprintf "fma {version}\\n"\n')
        f.chmod(0o755)
        return f.read_bytes()

    def run_installer(self, answer='', args=()):
        return subprocess.run(['bash', str(ROOT / 'install.sh'), *args], env=self.env,
                              input=answer, capture_output=True, text=True)

    def test_systemd_with_existing_binary(self):
        self.existing('1.2.0')
        generated = self.root / 'generated'
        generated.mkdir()
        config = self.root / 'config'
        units = self.root / 'units'
        import getpass
        (generated / 'install.mk').write_text(
            f'DEPLOY_USER := {getpass.getuser()}\nDEPLOY_UID := {os.getuid()}\n'
            f'BINDIR := {self.bin}\nCONFIG_DIR := {config}\nSYSTEMD_USER_DIR := {units}\n')
        for name in ['s3.env', 'outbound.env', 'fma.service']:
            (generated / name).write_text('# test configuration\n')
        for name, body in {
            'uname': 'echo Linux',
            'systemctl': 'printf "%s\\n" "$*" >> "$INSTALL_TEST_ROOT/systemctl.log"',
        }.items():
            script = self.mock / name
            script.write_text('#!/bin/bash\n' + body + '\n')
            script.chmod(0o755)
        # uname must still report the native CPU for archive selection.
        (self.mock / 'uname').write_text('#!/bin/bash\nif [[ "$1" == -s ]]; then echo Linux; else echo x86_64; fi\n')
        result = self.run_installer(args=('--systemd', '--config-dir', str(generated)))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((units / 'fma.service').exists())
        self.assertEqual((config / 's3.env').stat().st_mode & 0o777, 0o600)
        self.assertIn('--user restart fma.service', (self.root / 'systemctl.log').read_text())
        self.assertNotIn('/download/', (self.root / 'requests').read_text())

    def test_unknown_option(self):
        result = self.run_installer(args=('--unknown',))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / 'requests').exists())

    def test_first_install(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        version = subprocess.check_output([str(self.bin / 'fma'), '--version'], text=True)
        self.assertEqual(version, 'fma 1.2.0\n')
        self.assertEqual((self.bin / 'fma').stat().st_mode & 0o777, 0o755)

    def test_metadata_does_not_trigger_reinstall(self):
        previous = self.existing('1.2.0\\ncommit: abc123\\nrelease-time: 2026-09-10T12:00:00Z')
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.bin / 'fma').read_bytes(), previous)
        self.assertNotIn('/download/', (self.root / 'requests').read_text())

    def test_default_no_preserves_old_version(self):
        previous = self.existing('1.1.0')
        result = self.run_installer('\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('[y/N]', result.stderr)
        self.assertEqual((self.bin / 'fma').read_bytes(), previous)
        self.assertNotIn('/download/', (self.root / 'requests').read_text())

    def test_newer_version_is_not_downgraded(self):
        previous = self.existing('1.10.0')
        result = self.run_installer('y\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('[y/N]', result.stderr)
        self.assertEqual((self.bin / 'fma').read_bytes(), previous)

    def test_update_after_yes(self):
        self.existing('1.1.0')
        result = self.run_installer('y\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('1.2.0', (self.bin / 'fma').read_text())

    def test_bad_checksum_preserves_old_binary(self):
        previous = self.existing('1.1.0')
        with (self.root / 'archive.tar.gz').open('ab') as f:
            f.write(b'corruption')
        result = self.run_installer('y\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('SHA-256 mismatch', result.stderr)
        self.assertEqual((self.bin / 'fma').read_bytes(), previous)


if __name__ == '__main__':
    unittest.main()
