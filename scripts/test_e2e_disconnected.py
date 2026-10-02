"""Cluster-free tests for the disconnected caller's provisioning orchestration."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name("e2e-disconnected.sh")


class EntrypointTests(unittest.TestCase):
    def test_output_redacts_credentials(self):
        result = subprocess.run(
            ["python3", str(SCRIPT.with_name("e2e-redact.py"))],
            input="model output includes vllm-sensitive and hf-sensitive\n",
            env={**os.environ, "VLLM_API_KEY": "vllm-sensitive", "HUGGING_FACE_HUB_TOKEN": "hf-sensitive"},
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "model output includes [REDACTED] and [REDACTED]\n")

    def test_independent_cleanup_uses_invocation_label(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            oc = root / "oc"
            oc.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$COMMAND_LOG"\ncase "$*" in *"-o yaml"*) echo cleanup-sensitive ;; esac\n')
            oc.chmod(0o755)
            env = {
                **os.environ, "PATH": f"{root}:{os.environ['PATH']}",
                "E2E_DISCONNECTED_ID": "test-invocation", "VLLM_API_KEY": "cleanup-sensitive",
                "ARTIFACT_DIR": str(root / "artifacts"), "COMMAND_LOG": str(root / "commands"),
            }
            result = subprocess.run(["bash", str(SCRIPT.with_name("e2e-disconnected-cleanup.sh"))], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            commands = (root / "commands").read_text()
            self.assertIn("delete pods,secrets,networkpolicies,serviceaccounts,rolebindings", commands)
            self.assertIn("agentic.openshift.io/disconnected-e2e=test-invocation", commands)
            artifacts = (root / "artifacts/disconnected/fallback/owned-resources.yaml").read_text()
            self.assertIn("[REDACTED]", artifacts)
            self.assertNotIn("cleanup-sensitive", artifacts)

    def test_requires_full_service_commit(self):
        for ref in ("", "main", "abc123", "a" * 39, "a" * 41, "g" * 40, "a" * 40 + "^{commit}"):
            with self.subTest(ref=ref):
                result = subprocess.run(["bash", str(SCRIPT)], env={**os.environ, "LIGHTSPEED_SERVICE_REF": ref}, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("40 hexadecimal", result.stderr)


class OrchestrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.assets = self.root / "service/tests/rhoai"
        scripts = self.assets / "scripts"
        scripts.mkdir(parents=True)
        for name in ("nfd", "nvidia-operator"):
            path = self.assets / f"manifests/namespaces/{name}.yaml"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test\n")
        for name in ("vllm-runtime-gpu", "vllm-inference-service-gpu"):
            path = self.assets / f"manifests/vllm/{name}.yaml"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("metadata:\n  namespace: e2e-rhoai-dsc\n")
        for name in ("operators/operatorgroup", "operators/operators", "operators/ds-cluster", "gpu/create-nfd", "gpu/cluster-policy"):
            path = self.assets / f"manifests/{name}.yaml"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("metadata:\n  name: test\n")
        (scripts / "model-profile.sh").write_text('''load_vllm_model_profile() {
 [[ "$VLLM_MODEL_PROFILE" == gemma-4-31b ]] || return 1
 export VLLM_MODEL="${PROFILE_MODEL:-google/gemma-4-31B-it}"
 export VLLM_TOOL_CALL_PARSER="${PROFILE_PARSER:-gemma4}" VLLM_GPU_COUNT=4
 export VLLM_CHAT_TEMPLATE_URL=https://huggingface.co/google/gemma-4-31B-it/raw/main/chat_template.jinja
 export VLLM_CHAT_TEMPLATE_CONFIGMAP=vllm-chat-template
 export VLLM_CHAT_TEMPLATE_KEY=gemma-4-chat-template.jinja
}
''')
        for stage in ("bootstrap", "gpu-setup", "deploy-vllm", "get-vllm-pod-info"):
            path = scripts / f"{stage}.sh"
            self.write_executable(path, f'''#!/bin/bash
set -eu
printf '%s\\n' "{stage} profile=$VLLM_MODEL_PROFILE args=$*" >> "$STAGE_LOG"
[[ "${{FAIL_STAGE:-}}" != {stage} ]] || exit 19
if [[ "${{FAIL_STAGE:-}}" == hang && {stage} == bootstrap ]]; then
 sleep 86400 &
 echo $! > "$HANG_CHILD_PID"
 wait
fi
if [[ {stage} == deploy-vllm ]]; then [[ "$VLLM_IMAGE" == registry.example/vllm:test ]]; fi
if [[ {stage} == get-vllm-pod-info ]]; then
 printf '%s\\n' 'POD_NAME=vllm-pod' 'KSVC_URL=http://incorrect-target-port:8080' > "$ENV_FILE"
fi
''')
        (scripts / "fetch-vllm-image.sh").write_text('''printf '%s\n' "fetch-vllm-image profile=$VLLM_MODEL_PROFILE" >> "$STAGE_LOG"
export VLLM_IMAGE=registry.example/vllm:test
# Exercise redaction on output from sourced service assets.
echo "$HUGGING_FACE_HUB_TOKEN"
''')
        self.write_executable(self.bin / "git", '''#!/bin/bash
set -eu
if [[ "$1" == clone ]]; then
 cp -R "$SERVICE_FIXTURE" "${@: -1}"
elif [[ "$3" == cat-file ]]; then
 exit "${PIN_CHECK_RC:-0}"
elif [[ "$3" == rev-parse ]]; then
 echo "${GIT_HEAD:-$LIGHTSPEED_SERVICE_REF}"
fi
''')
        self.write_executable(self.bin / "curl", '''#!/usr/bin/env python3
import os, pathlib, sys
args = sys.argv[1:]
assert '--fail' in args and '--location' in args
assert os.environ['HUGGING_FACE_HUB_TOKEN'] not in ' '.join(args)
header = args[args.index('--header') + 1]
assert header.startswith('@')
assert pathlib.Path(header[1:]).read_text().strip() == 'Authorization: Bearer '+os.environ['HUGGING_FACE_HUB_TOKEN']
pathlib.Path(args[args.index('--output') + 1]).write_text('gemma chat template')
with open(os.environ['STAGE_LOG'], 'a') as f: f.write('chat-template\\n')
''')
        self.write_executable(self.bin / "oc", '''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ['OC_LOG'], 'a') as f: f.write(' '.join(args)+'\\n')
assert os.environ['VLLM_API_KEY'] not in args
assert os.environ['HUGGING_FACE_HUB_TOKEN'] not in args
if args[:2] == ['create', 'secret']:
    for arg in args:
        if arg.startswith('--from-file='):
            assert pathlib.Path(arg.split('=',2)[2]).is_file()
    print('{}')
elif args[:2] == ['create', 'configmap']:
    print('{}')
elif args[0] == 'apply' and args[-1] == '-':
    sys.stdin.read()
elif args[:2] == ['get', 'deployment']:
    sys.exit(0 if os.environ.get('DEPLOYMENT_EXISTS') == '1' else 1)
elif args[0] == 'logs' and '-l' in args:
    print('vLLM startup: unsupported gemma4 '+os.environ['VLLM_API_KEY'])
elif args[:2] == ['get', 'service']:
    print(os.environ['SERVICE_JSON'])
elif args[:2] == ['get', 'pods'] and '-l' in args:
    print(os.environ['PODS_JSON'])
elif args[:2] == ['get', 'agenticruns'] and 'json' in args:
    print('{"items":[]}')
elif args[0] == 'exec':
    import io, urllib.request, urllib.error
    assert 'VLLM_API_KEY' in ' '.join(args) or 'sys.stdin' in ' '.join(args)
    assert os.environ['VLLM_API_KEY'] not in ' '.join(args)
    expected_key = os.environ['VLLM_API_KEY']
    def models_api(request, **kwargs):
        assert request.full_url == 'http://vllm-model-predictor.e2e-rhoai-dsc.svc:8000/v1/models'
        assert request.get_header('Authorization') == 'Bearer '+expected_key
        if os.environ.get('MODELS_AUTH_FAILURE'):
            raise urllib.error.HTTPError(request.full_url, 401, 'Unauthorized', {}, None)
        data = {'data':[{'id':os.environ.get('MODELS_ID','google/gemma-4-31B-it')}]}
        return io.BytesIO(json.dumps(data).encode())
    urllib.request.urlopen = models_api
    source = args[args.index('python3')+2]
    sys.argv = ['models', args[-1]]
    os.environ['VLLM_API_KEY'] = os.environ.get('POD_ENV_KEY', expected_key)
    exec(compile(source, '<models-probe>', 'exec'), {'__name__':'__main__'})
elif args[0] == 'wait' and os.environ.get('FAIL_STAGE') == 'readiness':
    sys.exit(23)
''')
        self.write_executable(self.bin / "bash", '''#!/bin/bash
set -eu
if [[ "$1" == */e2e-disconnected-cleanup.sh && -n "${CLEANUP_CALLS:-}" ]]; then
 if [[ "$1" == "${TEST_CLUSTER_SCRIPT%/*}/e2e-disconnected-cleanup.sh" ]]; then
  echo inner >> "$CLEANUP_CALLS"
  exit "${INNER_CLEANUP_RC:-0}"
 fi
 echo outer >> "$CLEANUP_CALLS"
 exit "${OUTER_CLEANUP_RC:-0}"
fi
if [[ "$1" == */e2e-cluster.sh ]]; then
 [[ "$2" == openai && "$E2E_SCENARIO_TAGS" == core && "$E2E_DISCONNECTED" == true ]]
 [[ "$(< "$OPENAI_PROVIDER_KEY_PATH")" == test-secret ]]
 python3 - <<'PY'
import json, os
keys = ['E2E_OPENAI_URL','E2E_MODEL','RHOAI_VLLM_NAMESPACE','RHOAI_VLLM_SERVICE_NAME','RHOAI_VLLM_SERVICE_PORT','RHOAI_VLLM_NETWORK_PORT','RHOAI_VLLM_POD_SELECTOR_JSON']
with open(os.environ['ENTRYPOINT_REACHED'],'w') as f: json.dump({k:os.environ[k] for k in keys},f)
PY
 if [[ -n "${TEST_CLUSTER_SCRIPT:-}" ]]; then exec /bin/bash "$TEST_CLUSTER_SCRIPT" openai; fi
 exit "${TEST_SUITE_RC:-7}"
fi
exec /bin/bash "$@"
''')
        self.service = {
            "metadata": {"name": "vllm-model-predictor"},
            "spec": {
                "ports": [{"name": "http1", "protocol": "TCP", "port": 8000, "targetPort": 8080}],
                "selector": {"serving.kserve.io/inferenceservice": "vllm-model"},
            },
        }
        self.pods = {"items": [{
            "metadata": {"name": "vllm-pod"}, "status": {"phase": "Running"},
            "spec": {"containers": [{"name": "kserve-container", "ports": [{"name": "http1", "containerPort": 8080}]}]},
        }]}
        self.env = {
            **os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}",
            "LIGHTSPEED_SERVICE_REF": "a" * 40, "SERVICE_FIXTURE": str(self.root / "service"),
            "HUGGING_FACE_HUB_TOKEN": "hf-secret", "VLLM_API_KEY": "test-secret",
            "SANDBOX_IMAGE": "image-registry.openshift-image-registry.svc:5000/test/sandbox:v1",
            "ENTRYPOINT_REACHED": str(self.root / "reached"), "STAGE_LOG": str(self.root / "stages"),
            "OC_LOG": str(self.root / "oc-log"), "ARTIFACT_DIR": str(self.root / "artifacts"),
            "HANG_CHILD_PID": str(self.root / "sleep.pid"),
        }
        for name in ("E2E_PROVIDER_KEY_PATH", "OPENAI_PROVIDER_KEY_PATH", "E2E_SKIP_SCENARIOS", "E2E_SCENARIO_TAGS", "VLLM_MODEL_PROFILE", "PROFILE_MODEL", "PROFILE_PARSER", "FAIL_STAGE"):
            self.env.pop(name, None)

    @staticmethod
    def write_executable(path, content):
        path.write_text(content)
        path.chmod(0o755)

    def run_entrypoint(self, **overrides):
        env = {**self.env, "SERVICE_JSON": json.dumps(self.service), "PODS_JSON": json.dumps(self.pods), **overrides}
        with subprocess.Popen(["/bin/bash", str(SCRIPT)], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True) as process:
            try:
                stdout, stderr = process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
                raise
            return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def test_existing_assets_produce_handoff_and_preserve_suite_failure(self):
        self.assertFalse((self.assets / "scripts/provision-vllm.sh").exists())
        result = self.run_entrypoint()
        self.assertEqual(result.returncode, 7, result.stderr)
        handoff = json.loads((self.root / "reached").read_text())
        self.assertEqual(handoff["E2E_OPENAI_URL"], "http://vllm-model-predictor.e2e-rhoai-dsc.svc:8000/v1")
        self.assertEqual(handoff["E2E_MODEL"], "google/gemma-4-31B-it")
        self.assertEqual(handoff["RHOAI_VLLM_SERVICE_PORT"], "8000")
        self.assertEqual(handoff["RHOAI_VLLM_NETWORK_PORT"], "8080")
        self.assertEqual(json.loads(handoff["RHOAI_VLLM_POD_SELECTOR_JSON"]), {"matchLabels": self.service["spec"]["selector"]})
        stages = (self.root / "stages").read_text().splitlines()
        self.assertEqual([line.split()[0] for line in stages], ["bootstrap", "gpu-setup", "chat-template", "fetch-vllm-image", "deploy-vllm", "get-vllm-pod-info"])
        for line in stages:
            if not line.startswith("chat-template"):
                self.assertIn("profile=gemma-4-31b", line)
        commands = (self.root / "oc-log").read_text()
        self.assertIn("--for=condition=Ready", commands)
        self.assertIn("/v1", commands)
        self.assertNotIn("test-secret", result.stdout + result.stderr + commands)
        self.assertNotIn("hf-secret", result.stdout + result.stderr + commands)

    def test_product_cleanup_marker_controls_outer_fallback(self):
        scripts = self.root / "cluster/scripts"
        scripts.mkdir(parents=True)
        cluster = scripts / "e2e-cluster.sh"
        cluster.write_text(SCRIPT.with_name("e2e-cluster.sh").read_text())
        (scripts / "e2e-lib.sh").write_text('''log_info() { :; }
check_prerequisites() { exit "${TEST_SUITE_RC:-0}"; }
cleanup_e2e_otel() { :; }
cleanup_operator() { :; }
''')
        calls = self.root / "cleanup-calls"
        cases = (
            (0, 0, 11, 0, ["inner"]),
            (7, 0, 11, 7, ["inner"]),
            (0, 9, 0, 9, ["inner", "outer"]),
            (7, 9, 11, 7, ["inner", "outer"]),
        )
        for suite_rc, inner_rc, outer_rc, expected_rc, expected_calls in cases:
            with self.subTest(suite=suite_rc, inner=inner_rc, outer=outer_rc):
                calls.unlink(missing_ok=True)
                result = self.run_entrypoint(
                    TEST_CLUSTER_SCRIPT=str(cluster), CLEANUP_CALLS=str(calls),
                    TEST_SUITE_RC=str(suite_rc), INNER_CLEANUP_RC=str(inner_rc),
                    OUTER_CLEANUP_RC=str(outer_rc),
                )
                self.assertEqual(result.returncode, expected_rc, result.stdout + result.stderr)
                self.assertEqual(calls.read_text().splitlines(), expected_calls)

    def test_outer_cleanup_runs_when_product_runner_does_not_clean_up(self):
        calls = self.root / "cleanup-calls"
        result = self.run_entrypoint(
            TEST_CLUSTER_SCRIPT="", CLEANUP_CALLS=str(calls),
            TEST_SUITE_RC="0", OUTER_CLEANUP_RC="11",
        )
        self.assertEqual(result.returncode, 11, result.stdout + result.stderr)
        self.assertEqual(calls.read_text().splitlines(), ["outer"])

    def test_outer_cleanup_still_runs_after_provisioning_failure(self):
        calls = self.root / "cleanup-calls"
        result = self.run_entrypoint(
            TEST_CLUSTER_SCRIPT="", CLEANUP_CALLS=str(calls),
            FAIL_STAGE="bootstrap", OUTER_CLEANUP_RC="11",
        )
        self.assertEqual(result.returncode, 19, result.stdout + result.stderr)
        self.assertEqual(calls.read_text().splitlines(), ["outer"])

    def test_current_caller_key_and_existing_deployment_rollout(self):
        result = self.run_entrypoint(DEPLOYMENT_EXISTS="1", POD_ENV_KEY="old-pod-key")
        self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
        commands = (self.root / "oc-log").read_text()
        self.assertIn("rollout restart deployment/vllm-model-predictor", commands)
        self.assertIn("rollout status deployment/vllm-model-predictor", commands)
        self.assertNotIn("old-pod-key", result.stdout + result.stderr)

    def test_hung_provisioning_stage_is_bounded(self):
        result = self.run_entrypoint(FAIL_STAGE="hang", E2E_RHOAI_PROVISION_TIMEOUT="1s")
        self.assertEqual(result.returncode, 124, result.stdout + result.stderr)
        self.assertFalse((self.root / "reached").exists())
        child = (self.root / "sleep.pid").read_text().strip()
        stat = Path(f"/proc/{child}/stat")
        if stat.exists():
            self.assertEqual(stat.read_text().split()[2], "Z", "provisioning left a running child")

    def test_interruption_stops_connected_provisioning_children(self):
        env = {**self.env, "SERVICE_JSON": json.dumps(self.service), "PODS_JSON": json.dumps(self.pods), "FAIL_STAGE": "hang"}
        with subprocess.Popen(["/bin/bash", str(SCRIPT)], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True) as process:
            child_group = None
            try:
                deadline = time.monotonic() + 5
                marker = self.root / "sleep.pid"
                while not marker.exists() and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(marker.exists(), "provisioning did not reach the hung stage")
                child = marker.read_text().strip()
                child_group = os.getpgid(int(child))
                process.terminate()
                stdout, stderr = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 143, stdout + stderr)
                stat = Path(f"/proc/{child}/stat")
                if stat.exists():
                    self.assertEqual(stat.read_text().split()[2], "Z", "interruption left a running child")
            finally:
                if process.poll() is None:
                    if child_group is not None:
                        try:
                            os.killpg(child_group, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    os.killpg(process.pid, signal.SIGKILL)
                    process.communicate()
        self.assertFalse((self.root / "reached").exists())

    def test_provisioning_timeout_must_be_positive(self):
        for value in ("0", "0s", "0.0m", "invalid"):
            with self.subTest(timeout=value):
                result = self.run_entrypoint(E2E_RHOAI_PROVISION_TIMEOUT=value)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("positive GNU timeout duration", result.stdout + result.stderr)
                self.assertFalse((self.root / "stages").exists())

    def test_all_service_manifest_dependencies_are_validated_before_apply(self):
        for name in ("operators/operatorgroup", "operators/operators", "operators/ds-cluster", "gpu/create-nfd", "gpu/cluster-policy"):
            with self.subTest(manifest=name):
                path = self.assets / f"manifests/{name}.yaml"
                content = path.read_text()
                path.unlink()
                try:
                    result = self.run_entrypoint()
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    self.assertIn(f"manifests/{name}.yaml", result.stdout + result.stderr)
                    self.assertFalse((self.root / "stages").exists())
                    self.assertNotIn("apply", (self.root / "oc-log").read_text())
                finally:
                    path.write_text(content)

    def test_readiness_failure_archives_redacted_inference_logs(self):
        result = self.run_entrypoint(FAIL_STAGE="readiness")
        self.assertEqual(result.returncode, 23, result.stdout + result.stderr)
        log = (self.root / "artifacts/disconnected/fallback/inference.log").read_text()
        self.assertIn("unsupported gemma4", log)
        self.assertIn("[REDACTED]", log)
        self.assertNotIn("test-secret", log)
        self.assertFalse((self.root / "reached").exists())

    def test_named_target_port(self):
        self.service["spec"]["ports"][0]["targetPort"] = "http1"
        result = self.run_entrypoint()
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(json.loads((self.root / "reached").read_text())["RHOAI_VLLM_NETWORK_PORT"], "8080")

    def test_model_mismatch_stops_before_product_tests(self):
        result = self.run_entrypoint(MODELS_ID="meta-llama/Llama-3.1-8B-Instruct")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("model", (result.stdout + result.stderr).lower())
        self.assertFalse((self.root / "reached").exists())

    def test_models_api_auth_failure_stops_before_product_tests(self):
        result = self.run_entrypoint(MODELS_AUTH_FAILURE="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("401", result.stdout + result.stderr)
        self.assertFalse((self.root / "reached").exists())
        self.assertTrue((self.root / "artifacts/disconnected/fallback/inference.yaml").exists())

    def test_missing_asset_fails_before_cluster_changes(self):
        (self.assets / "scripts/model-profile.sh").unlink()
        result = self.run_entrypoint()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("model-profile.sh", result.stdout + result.stderr)
        self.assertFalse((self.root / "stages").exists())
        commands = (self.root / "oc-log").read_text()
        self.assertNotIn("apply", commands)
        self.assertNotIn("create secret", commands)

    def test_empty_service_selector_is_rejected(self):
        self.service["spec"]["selector"] = {}
        result = self.run_entrypoint()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("selector", (result.stdout + result.stderr).lower())
        self.assertFalse((self.root / "reached").exists())

    def test_incompatible_profile_fails_before_cluster_changes(self):
        result = self.run_entrypoint(PROFILE_PARSER="llama3_json")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Gemma profile", result.stdout + result.stderr)
        self.assertFalse((self.root / "stages").exists())
        self.assertNotIn("apply", (self.root / "oc-log").read_text())

    def test_unresolvable_named_port_is_rejected(self):
        self.service["spec"]["ports"][0]["targetPort"] = "missing"
        result = self.run_entrypoint()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("target port", result.stdout + result.stderr)
        self.assertFalse((self.root / "reached").exists())

    def test_invalid_service_port_is_rejected(self):
        self.service["spec"]["ports"][0]["port"] = 0
        result = self.run_entrypoint()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Service port", result.stdout + result.stderr)
        self.assertFalse((self.root / "reached").exists())

    def test_stage_and_pin_failures_are_preserved(self):
        for overrides, expected in (({"FAIL_STAGE": "bootstrap"}, 19), ({"FAIL_STAGE": "readiness"}, 23), ({"GIT_HEAD": "b" * 40}, 1), ({"PIN_CHECK_RC": "9"}, 9)):
            with self.subTest(overrides=overrides):
                result = self.run_entrypoint(**overrides)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertFalse((self.root / "reached").exists())


if __name__ == "__main__":
    unittest.main()
