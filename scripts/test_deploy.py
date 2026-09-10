#!/usr/bin/env python3
"""Exercise deployment generation without touching real configuration or services."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='fma-deploy-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        shutil.copytree(ROOT / 'deploy/template', self.root / 'deploy/template')
        shutil.copy2(ROOT / 'deploy/gen.sh', self.root / 'deploy/gen.sh')
        shutil.copy2(ROOT / 'Makefile', self.root / 'Makefile')

    def answers(self, mode='disabled', secret='test-secret'):
        answers = ['example.net', '', '', '/home/tester', '', '', '',
                   'https://s3.example.net', 'test-bucket', '', 'test-access', secret,
                   '', 'tls/fullchain.pem', 'tls/private.key', mode]
        if mode == 'relay':
            answers += ['smtp.example.net:587', '', 'test-user', '', secret, '']
        answers += ['', '', '', '', '', '', '', '', '', '', '']
        return answers

    def generate(self, answers):
        return subprocess.run(['bash', 'deploy/gen.sh'], cwd=self.root,
                              input='\n'.join(answers) + '\n', text=True,
                              capture_output=True)

    def test_generation_and_secret_escaping(self):
        # Includes shell substitution and template syntax: neither may execute.
        secret = 'a "quote" \'single\' $HOME `id` $(touch INJECTED) \\ & @@DOMAIN@@'
        result = self.generate(self.answers('relay', secret))
        self.assertEqual(result.returncode, 0, result.stderr)
        generated = self.root / 'deploy/generated'
        self.assertEqual(len(list(generated.iterdir())), 8)
        self.assertFalse((self.root / 'INJECTED').exists())
        service = (generated / 'fma.service').read_text()
        self.assertIn('-domain example.net', service)
        self.assertIn('-cert tls/fullchain.pem -key tls/private.key', service)
        self.assertIn('/home/tester/.local/bin/fma', service)
        self.assertIn('/home/tester/.config/fma/s3.env', service)
        for filename in ['fma.service', 'install.mk', 'nginx-http.conf',
                         'nginx-https.conf', 'nginx-stream.conf', 'renew-hook.sh']:
            self.assertNotIn('@@', (generated / filename).read_text())
        for filename, variable in [('s3.env', 'FMA_S3_SECRET_ACCESS_KEY'),
                                   ('outbound.env', 'FMA_RELAY_PASSWORD')]:
            # Variable names and paths are fixed by the test, never secret input.
            cmd = f'source deploy/generated/{filename}; printf "%s" "${variable}"'
            value = subprocess.run(['bash', '-c', cmd], cwd=self.root,
                                   capture_output=True, text=True, check=True)
            self.assertEqual(value.stdout, secret)
            self.assertEqual((generated / filename).stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.root / 'INJECTED').exists())
        self.assertEqual(generated.stat().st_mode & 0o777, 0o700)
        subprocess.run(['bash', '-n', str(generated / 'renew-hook.sh')], check=True)

    def test_cancellation_preserves_configuration(self):
        result = self.generate(self.answers())
        self.assertEqual(result.returncode, 0, result.stderr)
        generated = self.root / 'deploy/generated'
        before = {p.name: p.read_bytes() for p in generated.iterdir()}
        cancelled = self.generate(['different.example'])
        self.assertNotEqual(cancelled.returncode, 0)
        declined = self.generate(self.answers() + ['no'])
        self.assertEqual(declined.returncode, 0, declined.stderr)
        self.assertEqual(before, {p.name: p.read_bytes() for p in generated.iterdir()})

    def test_missing_config_stops_install_before_build(self):
        result = subprocess.run(['make', 'install'], cwd=self.root,
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('配置文件没有找到', result.stderr)
        self.assertNotIn('go build', result.stdout)
        self.assertNotIn('go vet', result.stdout)

    def test_invalid_port_does_not_generate(self):
        answers = self.answers()
        answers[17] = '70000'  # First loopback port, after retry delay.
        result = self.generate(answers)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / 'deploy/generated').exists())


if __name__ == '__main__':
    unittest.main()
