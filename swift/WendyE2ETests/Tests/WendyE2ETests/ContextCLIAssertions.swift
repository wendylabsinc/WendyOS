import Testing
import WendyE2ETesting

// Test-owned offline fixture and observer. The existing argv-only network counter
// cannot observe endpoints loaded from configuration. Python 3 is required on the CLI host.
enum ContextCLIAssertions {
    static func run(_ cli: WendyE2ESession, body: String) async throws {
        let home = try #require(cli.env["HOME"])
        try #require(
            cli.workingDirectory == home + (cli.machine.os == .windows ? "\\work" : "/work")
                || cli.workingDirectory == home + "/work"
        )
        let program =
            fixture + "\n"
            + body.split(separator: "\n", omittingEmptySubsequences: false)
            .map { "    " + $0 }.joined(separator: "\n") + "\n"
        try await cli.sh(
            posix: "python3 -c " + LocalCLIAssertions.quote(program),
            power: "python -c " + LocalCLIAssertions.powerQuote(program)
        )
    }

    private static let fixture = #"""
        import copy, json, os, pathlib, shutil, socket, subprocess, threading

        if not __debug__:
            raise RuntimeError("Context specs require Python assertions; disable Python optimization")

        # Only public self-signed certificates. Their ephemeral signing keys were discarded.
        # Wide fixed bounds avoid dependence on the test day's hour or a renewal window.
        VALID_CERT = '-----BEGIN CERTIFICATE-----\nMIHuMIGWoAMCAQICAicPMAoGCCqGSM49BAMCMAAwIBcNMDAwMTAxMDAwMDAwWhgP\nOTk5OTAxMDEwMDAwMDBaMAAwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAR+FZf5\n71BlHUEIlt9zRhmDH++NylMBtCjIhWDyf+0qT7cIXdjq26ZjE4OQI2Utw37Okpod\nzRxWWihxQpP5gbf2MAoGCCqGSM49BAMCA0cAMEQCIDfsoWMW6sQad2+qFKEXjySh\nTHmMfTo4KKhDv/uP9Jw/AiBRybjOg5GTj+g6T4ZrtUhITD6gSaT0Jkc3Xwuh0cTb\nSQ==\n-----END CERTIFICATE-----\n'
        EXPIRED_CERT = '-----BEGIN CERTIFICATE-----\nMIHtMIGUoAMCAQICAgfRMAoGCCqGSM49BAMCMAAwHhcNMDAwMTAxMDAwMDAwWhcN\nMDEwMTAxMDAwMDAwWjAAMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEXHV/JaUB\nHPwyy7oMSrBqcOTFKMzS4IuH1PSKaijywhEilZd02ybzEDshvcyAHYK3JLkceo7o\nlHVI5sI07QpBHDAKBggqhkjOPQQDAgNIADBFAiEA1B5kh88lgisfwFt5R/wAONf2\nwu/6wwXfE6a/HeaMF3UCIEkKIXEUVCvlPkse908r9xNZ+eUikTGW8MlrWeTPli6a\n-----END CERTIFICATE-----\n'

        class ContextFixture:
            def __enter__(self):
                self.listener = socket.socket()
                self.listener.bind(('127.0.0.1', 0))
                self.listener.listen()
                self.endpoint = '%s:%d' % self.listener.getsockname()
                self.url = 'http://' + self.endpoint
                self.directory = pathlib.Path(os.environ['HOME']) / '.wendy'
                self.directory.mkdir(parents=True, exist_ok=True)
                self.path = self.directory / 'config.json'
                self.calls = 0
                self.positive_control = 0
                # Synchronously accept a real TCP connection before observing the CLI.
                with socket.create_connection(self.listener.getsockname(), timeout=2):
                    connection, _ = self.listener.accept()
                    connection.close()
                    self.positive_control += 1
                self.listener.settimeout(0.05)
                self.stop = threading.Event()
                self.observer_errors = []
                def observe():
                    try:
                        while not self.stop.is_set():
                            try:
                                connection, _ = self.listener.accept()
                            except socket.timeout:
                                continue
                            self.calls += 1
                            connection.close()
                    except BaseException as error:
                        self.observer_errors.append(type(error).__name__)
                self.thread = threading.Thread(target=observe)
                self.original = {
                    'currentContext': 'alpha', 'defaultBuildHost': 'build-host-preference-SENTINEL',
                    'analytics': {'enabled': False}, 'completionPromptDismissed': True,
                    'lastCompletionPromptCheck': '2026-01-01T00:00:00Z',
                    'auth': [self.account('alpha', 41, VALID_CERT), self.account('beta', 73, EXPIRED_CERT)]}
                self.write(self.original)
                (self.directory / 'preserve.txt').write_text('unrelated configuration SENTINEL')
                self.thread.start()
                return self

            def __exit__(self, kind, value, traceback):
                self.stop.set()
                self.thread.join(timeout=2)
                # Drain handshakes queued just before the CLI exited as well.
                self.listener.setblocking(False)
                while True:
                    try:
                        connection, _ = self.listener.accept()
                    except BlockingIOError:
                        break
                    self.calls += 1
                    connection.close()
                self.listener.close()
                assert not self.thread.is_alive(), 'observer did not stop'
                assert not self.observer_errors, 'observer failed'
                print(json.dumps({'observerPositiveControl': self.positive_control, 'configuredEndpointConnections': self.calls}))
                assert self.positive_control == 1, 'observer positive control missing'
                assert self.calls == 0, 'CLI contacted a saved endpoint: %d connections' % self.calls

            def account(self, name, org, certificate):
                return {
                    'name': name, 'cloudDashboard': self.url, 'cloudGRPC': self.endpoint,
                    'oauthIssuer': self.url + '/realm/fixture', 'oauthClientId': 'fixture-client',
                    'oauthResource': self.url + '/cloud', 'pkiResource': self.url + '/pki',
                    'pkiEndpoint': self.url + '/pki', 'oauthExpiresAt': '2001-01-01T00:00:00Z',
                    'apiKey': name + '-API-SENTINEL', 'refreshToken': name + '-REFRESH-SENTINEL',
                    'dpopPrivateKey': name + '-DPOP-PRIVATE-SENTINEL',
                    'litePrivateKey': name + '-LITE-PRIVATE-SENTINEL',
                    'registryPrivateKey': name + '-REGISTRY-PRIVATE-SENTINEL',
                    'registryKeyId': name + '-registry-id',
                    'certificates': [{'organizationId': org, 'userId': name + '-user',
                        'principalUri': 'fixture:' + name, 'pemCertificate': certificate,
                        'pemCertificateChain': certificate, 'pemPrivateKey': name + '-PEM-PRIVATE-SENTINEL'}]}

            def write(self, value):
                self.path.write_text(json.dumps(value, indent=1) + '\n')

            def snapshot(self):
                return {str(p.relative_to(self.directory)): p.read_bytes()
                        for p in self.directory.rglob('*') if p.is_file()}

            def expect_config(self, expected):
                actual = json.loads(self.path.read_text())
                # Do not dump credential dictionaries when a preservation assertion fails.
                assert actual == expected, 'saved configuration differs from the independently specified state'
                assert set(self.snapshot()) == {'config.json', 'preserve.txt'}, 'unexpected config files'
                assert (self.directory / 'preserve.txt').read_text() == 'unrelated configuration SENTINEL'

            def label(self, name):
                return 'org %d — %s' % ({'alpha': 41, 'beta': 73}[name], self.endpoint)

            def run(self, *args, exit=0):
                env = {k: os.environ[k] for k in ('HOME', 'PATH', 'GOCOVERDIR', 'TMPDIR', 'TEMP', 'TMP', 'SystemRoot', 'USERPROFILE') if k in os.environ}
                env.update(WENDY_CONFIG_DIR=str(self.directory), WENDY_ANALYTICS='false',
                           WENDY_NO_UPDATE_CHECK='1', WENDY_SECRET_STORE='file', CI='1', NO_COLOR='1')
                # DEVNULL on stdin and pipes on both output streams guarantee no terminal.
                # TimeoutExpired is a harness failure, never an expected command error.
                result = subprocess.run([shutil.which('wendy'), *args], env=env,
                                        stdin=subprocess.DEVNULL, capture_output=True, text=True, encoding="utf-8", timeout=8)
                output = result.stdout + result.stderr
                for name in ('alpha', 'beta'):
                    for secret in ('API', 'REFRESH', 'DPOP-PRIVATE', 'LITE-PRIVATE', 'REGISTRY-PRIVATE', 'PEM-PRIVATE'):
                        assert name + '-' + secret + '-SENTINEL' not in output, 'secret value disclosed'
                assert '-----BEGIN PRIVATE KEY-----' not in output, 'private key disclosed'
                assert '\x1b' not in output, 'terminal escape in noninteractive output'
                assert result.returncode == exit, 'CLI exit %d, expected %d: %s' % (result.returncode, exit, output)
                if exit == 0:
                    assert result.stderr == '', 'unexpected stderr: ' + result.stderr
                else:
                    assert result.stdout == '', 'error wrote stdout'
                    assert 'could not open a new TTY' not in result.stderr, 'picker attempted'
                return result

            def expect_status(self, result, current='alpha', names=('alpha', 'beta')):
                expected = []
                for index, (name, org, expires, expired) in enumerate(zip(names, (41, 73),
                        ('9999-01-01T00:00:00Z', '2001-01-01T00:00:00Z'), (False, True))):
                    session = {'context': name, 'cloud': self.url, 'cloudGrpc': self.endpoint,
                        'userId': ('alpha', 'beta')[index] + '-user', 'organizationId': org,
                        'principalUri': 'fixture:' + ('alpha', 'beta')[index], 'loginPath': 'OIDC',
                        'certificate': {'expiresAt': expires, 'expired': expired,
                                        'expiringSoon': False, 'keyAlgorithm': 'ECDSA P-256'}}
                    if name == current:
                        session['current'] = True
                    expected.append(session)
                assert json.loads(result.stdout) == {'loggedIn': True, 'sessions': expected}, 'incorrect status facts'

        with ContextFixture() as f:
        """#
}
