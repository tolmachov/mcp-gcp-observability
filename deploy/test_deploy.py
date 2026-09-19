"""Run deployment entrypoints against fake CLIs; never contact GCP."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

DEPLOY = Path(__file__).resolve().parent

FAKE_CLI = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ["FAKE_ROOT"])
state_file = root / "state.json"
state = json.loads(state_file.read_text())
with (root / "calls.jsonl").open("a") as f:
    f.write(json.dumps([name, *args]) + "\n")
def save():
    state_file.write_text(json.dumps(state))
def value(flag):
    for i, arg in enumerate(args):
        if arg == flag:
            return args[i+1]
        if arg.startswith(flag+"="):
            return arg.split("=", 1)[1]
    return ""
def fail(kind):
    return os.environ.get("FAIL") == kind and state.get("stage", 0) == int(os.environ.get("FAIL_STAGE", "1"))
if name == "alert-check":
    sys.exit(1 if fail("alert") else 0)
if name == "curl":
    candidate = args[-1].endswith("/__candidate/readyz")
    if (candidate and os.environ.get("FAIL") == "candidate") or (not candidate and (fail("readiness") or os.environ.get("FAIL") == "legacy")):
        sys.exit(22)
    sys.exit(0)
cmd = " ".join(args[:3])
if cmd == "run services describe":
    traffic = [{"revisionName": k, "percent": v} for k, v in state["traffic"].items()]
    traffic.append({"revisionName": state["candidate"], "tag": "candidate"})
    print(json.dumps({"metadata": {"annotations": {"run.googleapis.com/ingress": "internal-and-cloud-load-balancing"}},
        "spec": {"template": {"metadata": {"annotations": {"run.googleapis.com/execution-environment": "gen2", "autoscaling.knative.dev/minScale": "1", "autoscaling.knative.dev/maxScale": "3"}},
        "spec": {"timeoutSeconds": 600, "containerConcurrency": 8, "serviceAccountName": "runtime@example.iam.gserviceaccount.com", "containers": [{"resources": {"limits": {"cpu": "1", "memory": "1Gi"}}}]}}}, "status": {"traffic": traffic}}))
elif args[:2] == ["run", "deploy"]:
    state["candidate"] = "candidate-rev"
    save()
elif cmd == "run services update-traffic":
    target = value("--to-revisions")
    if target != "stable-rev=100":
        state["stage"] += 1
    state["traffic"] = {r.split("=")[0]: int(r.split("=")[1]) for r in target.split(",")}
    save()
    if target != "stable-rev=100" and fail("traffic"):
        sys.exit(1)
elif args[:2] == ["logging", "read"]:
    if fail("logging"):
        sys.exit(1)
    if fail("event"):
        print("2026-09-19T00:00:00Z")
elif cmd == "compute url-maps describe":
    if value("--format") == "json":
        m = {"name": "mcp-cutover" if state["routes"] == "candidate" else "mcp-routes", "defaultService": "/backendServices/mcp-obs-backend", "pathRules": [{"paths": ["/__candidate/readyz"], "service": "/backendServices/mcp-obs-candidate-backend"}]}
        if state["routes"] == "candidate":
            m["pathRules"].append({"paths": ["/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", "/jwks.json", "/register", "/authorize", "/authorize/confirm", "/callback", "/token", "/revoke"], "service": "/backendServices/mcp-obs-candidate-backend"})
        if os.environ.get("UNOWNED_ROUTE"):
            m["routeRules"] = [{"priority": 1}]
        print(json.dumps({"hostRules": [{"hosts": ["mcp.example.com"], "pathMatcher": m["name"]}], "pathMatchers": [m]}))
    else:
        print("/backendServices/mcp-obs-backend")
elif cmd == "compute url-maps add-path-matcher":
    state["routes"] = "candidate" if value("--path-matcher-name") == "mcp-cutover" else "stable"
    save()
elif cmd == "compute network-endpoint-groups describe":
    print("mcp-gcp-observability\tcandidate" if "cloudRun.tag" in value("--format") else "mcp-gcp-observability")
elif cmd == "compute backend-services describe":
    fmt = value("--format")
    if "backends.group" in fmt:
        print("/networkEndpointGroups/" + ("mcp-obs-candidate-neg" if args[3] == "mcp-obs-candidate-backend" else "mcp-obs-neg"))
    elif "loadBalancingScheme" in fmt:
        print("EXTERNAL_MANAGED")
    elif "securityPolicy" in fmt:
        print("/securityPolicies/mcp-obs-armor")
elif args[:4] == ["compute", "security-policies", "rules", "describe"]:
    if os.environ.get("MISSING_RULE") == args[4]:
        sys.exit(1)
    if "action" in value("--format"):
        print("rate-based-ban" if args[4] == "100" else "throttle")
    elif "rateLimitOptions" in value("--format"):
        print("60\t60\t300\t300\t600\tIP" if args[4] == "100" else "600\t60\tIP")
elif cmd == "compute ssl-certificates describe":
    print("mcp.example.com")
elif cmd == "compute target-https-proxies describe":
    print("/urlMaps/mcp-obs-url-map")
elif cmd == "compute addresses describe" or cmd == "dns record-sets describe":
    print("192.0.2.1")
elif cmd in ("compute security-policies describe", "compute forwarding-rules describe"):
    pass
else:
    sys.exit("unexpected fake gcloud invocation: " + repr(args))
'''


class DeployTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for cli in ("gcloud", "curl", "alert-check"):
            path = self.root / cli
            path.write_text(FAKE_CLI)
            path.chmod(0o755)
        self.state_file = self.root / "state.json"
        self.state_file.write_text(json.dumps({"traffic": {"stable-rev": 100}, "candidate": "candidate-rev", "routes": "stable", "stage": 0}))
        env_file = self.root / "cloudrun.env"
        env_file.write_text("MCP_TRANSPORT: http\n")
        self.env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ["PATH"],
                        FAKE_ROOT=str(self.root), PUBLIC_HOSTNAME="mcp.example.com", DNS_ZONE="example",
                        FIRESTORE_LOCATION="eur3", GCP_PROJECT="test-project", GCP_REGION="europe-west1",
                        SERVICE="mcp-gcp-observability", RESOURCE_PREFIX="mcp-obs", RUNTIME_SA="runtime@example.iam.gserviceaccount.com",
                        ENV_FILE=str(env_file), IMAGE="example/image:v2", OPERATOR_BEARER_TOKEN="test-token",
                        CANDIDATE_REVISION="candidate-rev", ALERT_CHECK_COMMAND="alert-check", ROLLOUT_DWELL_SECONDS="0")
        for key in ("FAIL", "FAIL_STAGE", "UNOWNED_ROUTE", "MISSING_RULE"):
            self.env.pop(key, None)

    def run_script(self, script, action, success=True):
        result = subprocess.run(["bash", str(DEPLOY / script), action], env=self.env, capture_output=True, text=True, timeout=30)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def state(self):
        return json.loads(self.state_file.read_text())

    def calls(self):
        return [json.loads(line) for line in (self.root / "calls.jsonl").read_text().splitlines()]

    def traffic_targets(self):
        return [c[c.index("--to-revisions") + 1] for c in self.calls() if c[1:4] == ["run", "services", "update-traffic"]]

    def test_compatible_rollout(self):
        self.run_script("rollout.sh", "apply")
        self.assertEqual(self.traffic_targets(), ["candidate-rev=10,stable-rev=90", "candidate-rev=50,stable-rev=50", "candidate-rev=100"])

    def test_failure_at_every_stage_rolls_back_once(self):
        for failure in ("logging", "alert", "readiness", "event", "traffic"):
            for stage in (1, 2, 3):
                with self.subTest(failure=failure, stage=stage):
                    self.state_file.write_text(json.dumps({"traffic": {"stable-rev": 100}, "candidate": "candidate-rev", "routes": "stable", "stage": 0}))
                    (self.root / "calls.jsonl").write_text("")
                    self.env.update(FAIL=failure, FAIL_STAGE=str(stage))
                    self.run_script("rollout.sh", "apply", success=False)
                    self.assertEqual(self.state()["traffic"], {"stable-rev": 100})
                    self.assertEqual(self.traffic_targets().count("stable-rev=100"), 1)

    def test_prepare_needs_no_token_and_does_not_promote(self):
        del self.env["OPERATOR_BEARER_TOKEN"]
        del self.env["ALERT_CHECK_COMMAND"]
        self.run_script("rollout.sh", "prepare")
        self.assertEqual(self.traffic_targets(), [])
        self.assertFalse(any(c[0] == "curl" for c in self.calls()))

    def test_termination_rolls_back(self):
        self.env["ALERT_CHECK_COMMAND"] = 'kill -TERM "$PPID"'
        result = self.run_script("rollout.sh", "apply", success=False)
        self.assertEqual(result.returncode, 143)
        self.assertEqual(self.traffic_targets()[-1], "stable-rev=100")

    def test_first_v2_cutover(self):
        self.run_script("rollout.sh", "prepare")
        self.run_script("oauth-routes.sh", "candidate")
        route_call = next(c for c in self.calls() if c[1:4] == ["compute", "url-maps", "add-path-matcher"])
        rules = route_call[route_call.index("--path-rules") + 1]
        for path in ("/register", "/authorize", "/authorize/confirm", "/callback", "/token", "/revoke", "/.well-known/oauth-authorization-server"):
            self.assertIn(path + "=mcp-obs-candidate-backend", rules)
        self.run_script("rollout.sh", "cutover")
        self.assertEqual(self.traffic_targets(), ["candidate-rev=100"])
        self.assertEqual(self.state()["routes"], "stable")
        self.assertEqual(sum(c[1:3] == ["run", "deploy"] for c in self.calls()), 1)

    def test_failed_cutover_restores_traffic_and_oauth(self):
        for failure in ("candidate", "alert", "logging", "readiness"):
            with self.subTest(failure=failure):
                self.state_file.write_text(json.dumps({"traffic": {"stable-rev": 100}, "candidate": "candidate-rev", "routes": "candidate", "stage": 0}))
                self.env["FAIL"] = failure
                self.run_script("rollout.sh", "cutover", success=False)
                self.assertEqual(self.state()["traffic"], {"stable-rev": 100})
                self.assertEqual(self.state()["routes"], "stable")

    def test_legacy_token_cannot_start_canary(self):
        self.env["FAIL"] = "legacy"
        self.run_script("rollout.sh", "apply", success=False)
        self.assertEqual(self.traffic_targets(), [])
        self.assertFalse(any(c[1:3] == ["run", "deploy"] for c in self.calls()))

    def test_candidate_tag_mismatch_is_rejected(self):
        self.env["CANDIDATE_REVISION"] = "unexpected"
        self.run_script("oauth-routes.sh", "candidate", success=False)
        self.assertEqual(self.state()["routes"], "stable")
        self.run_script("rollout.sh", "cutover", success=False)
        self.assertEqual(self.traffic_targets(), [])

    def test_canary_refuses_pending_cutover(self):
        self.run_script("oauth-routes.sh", "candidate")
        self.run_script("rollout.sh", "apply", success=False)
        self.assertEqual(self.traffic_targets(), [])
        self.assertFalse(any(c[1:3] == ["run", "deploy"] for c in self.calls()))

    def test_unowned_routes_are_not_overwritten(self):
        self.env["UNOWNED_ROUTE"] = "1"
        self.run_script("oauth-routes.sh", "candidate", success=False)
        self.assertFalse(any(c[1:4] == ["compute", "url-maps", "add-path-matcher"] for c in self.calls()))

    def test_edge_check_existing_rules_is_read_only(self):
        self.run_script("edge.sh", "check")
        for call in self.calls():
            self.assertIn("describe", call)

    def test_edge_check_missing_rule_fails(self):
        self.env["MISSING_RULE"] = "100"
        self.run_script("edge.sh", "check", success=False)
        self.assertFalse(any("create" in c or "update" in c for c in self.calls()))


if __name__ == "__main__":
    unittest.main()
