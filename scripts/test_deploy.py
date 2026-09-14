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
        answers = ['example.net', 'tester', '1000', '/home/tester', '', '', '',
                   'https://s3.example.net', 'test-bucket', '', 'test-access', secret,
                   '', 'tls/fullchain.pem', 'tls/private.key', mode]
        if mode == 'relay':
            answers += ['smtp.example.net:587', '', 'test-user', '', secret, '']
        answers += ['', '', '', '', '', '', '', '', '', '', '']
        return answers + ['', '']

    def generate(self, answers):
        return subprocess.run(['bash', 'deploy/gen.sh'], cwd=self.root,
                              input='\n'.join(answers) + '\n', text=True,
                              capture_output=True)

    def test_generation_and_secret_escaping(self):
        # Includes shell substitution and template syntax: neither may execute.
        secret = 'a "quote" \'single\' $HOME `id` $(touch INJECTED) \\ & @@MAIL_HOST@@'
        result = self.generate(self.answers('relay', secret))
        self.assertEqual(result.returncode, 0, result.stderr)
        generated = self.root / 'deploy/generated'
        self.assertEqual(len(list(generated.iterdir())), 8)
        self.assertFalse((self.root / 'INJECTED').exists())
        service = (generated / 'fma.service').read_text()
        self.assertNotIn('-domain', service)
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

    def test_invalid_input_reprompts_and_defaults(self):
        answers = self.answers()
        answers[0] = '  EXAMPLE.NET  '
        for index, bad in sorted([(0, 'https://example.net'), (7, 'http://999.1.2.3:9000'),
                                  (8, 'bad/bucket'), (9, 'not a region'),
                                  (17, '70000'), (18, '2525')], reverse=True):
            answers.insert(index, bad)
        result = self.generate(answers)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertGreaterEqual(result.stderr.count('Please try again'), 6)
        generated = self.root / 'deploy/generated'
        self.assertNotIn('-domain', (generated / 'fma.service').read_text())
        self.assertIn('us-east-1', (generated / 's3.env').read_text())

    def test_noninteractive_environment(self):
        env = {**os.environ, 'FMA_MAIL_HOST': 'mail.example.net',
               'FMA_DEPLOY_USER': 'tester', 'FMA_DEPLOY_UID': '1000',
               'FMA_DEPLOY_HOME': '/home/tester', 'FMA_S3_ACCESS_KEY_ID': 'access',
               'FMA_S3_SECRET_ACCESS_KEY': 'do-not-print-this-secret'}
        result = subprocess.run(['bash', 'deploy/gen.sh', '--non-interactive'],
                                cwd=self.root, env=env, input='', capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('do-not-print-this-secret', result.stdout + result.stderr)
        generated = self.root / 'deploy/generated'
        self.assertNotIn('-domain', (generated / 'fma.service').read_text())
        self.assertIn('us-east-1', (generated / 's3.env').read_text())
        env['FMA_S3_ENDPOINT'] = 'http://999.0.0.1'
        result = subprocess.run(['bash', 'deploy/gen.sh', '--non-interactive'],
                                cwd=self.root, env=env, input='', capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Invalid or missing FMA_S3_ENDPOINT', result.stderr)

    def test_root_service_defaults(self):
        answers = self.answers()
        answers[1:4] = ['root', '0', '/root']
        result = self.generate(answers)
        self.assertEqual(result.returncode, 0, result.stderr)
        generated = self.root / 'deploy/generated'
        service = (generated / 'fma.service').read_text()
        self.assertIn('ExecStart=/usr/local/bin/fma', service)
        self.assertIn('EnvironmentFile=/etc/fma/s3.env', service)
        self.assertIn('WantedBy=multi-user.target', service)
        self.assertIn('SYSTEMD_USER_DIR := /etc/systemd/system', (generated / 'install.mk').read_text())
        self.assertIn('SYSTEMD_FLAGS := \n', (generated / 'install.mk').read_text())

    def test_endpoint_parsing(self):
        for endpoint in ['http://127.0.0.1:9000', 'https://s3.example.net',
                         'http://[::1]:9000', 'http://[2001:db8::1]/s3']:
            with self.subTest(endpoint=endpoint):
                answers = self.answers()
                answers[7] = endpoint
                result = self.generate(answers + ['yes'])
                self.assertEqual(result.returncode, 0, result.stderr)
        for endpoint in ['https://', 'http://bad_host', 'http://[1::2::3]',
                         'http://[:1::]', 'http://host.example:65536', 'http://user@host.example']:
            with self.subTest(endpoint=endpoint):
                answers = self.answers()
                answers.insert(7, endpoint)
                result = self.generate(answers + ['yes'])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('Invalid S3_ENDPOINT', result.stderr)

    def test_invalid_port_does_not_generate(self):
        answers = self.answers()
        answers[17] = '70000'  # First loopback port, after retry delay.
        result = self.generate(answers)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / 'deploy/generated').exists())


if __name__ == '__main__':
    unittest.main()
