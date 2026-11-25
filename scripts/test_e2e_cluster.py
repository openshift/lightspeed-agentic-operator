"""Cluster-free tests for the connected product E2E runner (e2e-cluster.sh)."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).parent
KEY = "sk-ant-test-sensitive"

# Stub library: no cluster work; collect_artifacts leaks the key like a real
# operator or sandbox log could.
STUB_LIB = '''
log_info() { echo "INFO: $*" >&2; }
log_error() { echo "ERROR: $*" >&2; }
check_prerequisites() { :; }
modelversions() { :; }
parse_snapshot() { :; }
deploy_operator() { :; }
ensure_e2e_otel() { :; }
cleanup_e2e_otel() { :; }
cleanup_operator() { :; }
resolve_model() { echo claude-haiku-5-5; }
collect_artifacts() {
    mkdir -p "$ARTIFACT_DIR/$1/podlogs"
    cat "$ANTHROPIC_PROVIDER_KEY_PATH" > "$ARTIFACT_DIR/$1/podlogs/sandbox-ls-1.log"
}
'''

# Stub `go test`: records the provider env and prints the key, simulating a
# leak through test output.
STUB_GO = '''#!/bin/bash
printf '%s %s %s\\n' "$E2E_PROVIDER" "$E2E_MODEL" "$E2E_PROVIDER_KEY_PATH" > "$GO_ENV_LOG"
echo "request header x-api-key: $(cat "$E2E_PROVIDER_KEY_PATH")"
'''

STUB_GIT = '''#!/bin/bash
mkdir -p "${@: -1}"
'''


class AnthropicRouteTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        scripts = self.root / "repo/scripts"
        scripts.mkdir(parents=True)
        for name in ("e2e-cluster.sh", "e2e-redact.py"):
            (scripts / name).write_text((SCRIPTS / name).read_text())
        (scripts / "e2e-lib.sh").write_text(STUB_LIB)
        self.cluster = scripts / "e2e-cluster.sh"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name, body in (("go", STUB_GO), ("git", STUB_GIT)):
            path = self.bin / name
            path.write_text(body)
            path.chmod(0o755)
        self.key_path = self.root / "anthropic-apitoken"
        self.key_path.write_text(KEY + "\n")
        self.artifacts = self.root / "artifacts"
        self.go_env = self.root / "go-env"

    def tearDown(self):
        self.tmp.cleanup()

    def run_cluster(self, *providers, **env):
        inherited = {k: v for k, v in os.environ.items() if k not in ("E2E_DISCONNECTED", "ANTHROPIC_PROVIDER_KEY_PATH")}
        full_env = {
            **inherited,
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "ARTIFACT_DIR": str(self.artifacts),
            "GO_ENV_LOG": str(self.go_env),
            **env,
        }
        return subprocess.run(["bash", str(self.cluster), *providers], env=full_env, capture_output=True, text=True)

    def test_anthropic_reads_key_from_provider_key_path(self):
        result = self.run_cluster("anthropic", ANTHROPIC_PROVIDER_KEY_PATH=str(self.key_path))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.go_env.read_text().split(), ["anthropic", "claude-haiku-5-5", str(self.key_path)])

    def test_anthropic_requires_provider_key_path(self):
        result = self.run_cluster("anthropic")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ANTHROPIC_PROVIDER_KEY_PATH", result.stderr)
        self.assertFalse(self.go_env.exists())

    def test_anthropic_key_is_redacted_from_output_and_artifacts(self):
        result = self.run_cluster("anthropic", ANTHROPIC_PROVIDER_KEY_PATH=str(self.key_path))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn(KEY, result.stdout + result.stderr)
        self.assertIn("x-api-key: [REDACTED]", result.stdout)
        files = [p for p in (self.artifacts / "anthropic").rglob("*") if p.is_file()]
        self.assertEqual(
            sorted(p.name for p in files), ["product-e2e-output.log", "sandbox-ls-1.log"],
        )
        for path in files:
            self.assertNotIn(KEY, path.read_text(), path)
            self.assertIn("[REDACTED]", path.read_text(), path)

    def test_default_provider_list_includes_anthropic(self):
        result = self.run_cluster()
        for line in result.stderr.splitlines():
            if line.startswith("INFO: Providers:"):
                self.assertIn("anthropic", line.split(":", 2)[2].split())
                return
        self.fail("Providers log line not found:\n" + result.stderr)


class RedactInPlaceTests(unittest.TestCase):
    def test_in_place_redacts_provider_key_from_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            key_path = root / "key"
            key_path.write_text(f"  {KEY}\n")
            leaked, clean = root / "leaked.log", root / "clean.log"
            leaked.write_text(f"token={KEY}\n")
            clean.write_text("nothing here\n")
            result = subprocess.run(
                ["python3", str(SCRIPTS / "e2e-redact.py"), "--in-place", str(leaked), str(clean)],
                env={**os.environ, "E2E_PROVIDER_KEY_PATH": str(key_path)}, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(leaked.read_text(), "token=[REDACTED]\n")
            self.assertEqual(clean.read_text(), "nothing here\n")


if __name__ == "__main__":
    unittest.main()
