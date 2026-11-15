"""Local unit and subprocess tests for ProgressReporter preservation and supervisor hot-upgrade."""
import fcntl
import hashlib
import importlib.util
import json
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

TEMPLATES = pathlib.Path(__file__).resolve().parents[1] / "templates"
sys.path.insert(0, str(TEMPLATES))
from studio_progress import ProgressReporter


class ProgressPreservationTests(unittest.TestCase):
    def test_preserves_verified_progress_on_supervisor_restart(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            prior = {
                "phase": "ready",
                "fileIndex": 16,
                "totalFiles": 16,
                "verifiedFiles": 16,
                "completedBytes": 1000,
                "totalBytes": 1000,
                "currentFile": "last.safetensors",
                "currentBytes": 500,
                "fileBytes": 500,
                "speedBytes": 123.0,
                "etaSeconds": 5.0,
            }
            (root / "progress.json").write_text(json.dumps(prior), encoding="utf-8")

            reporter = ProgressReporter(root)
            reporter.set(phase="repositories")

            saved = json.loads((root / "progress.json").read_text(encoding="utf-8"))
            self.assertEqual(saved["phase"], "repositories")
            self.assertEqual(saved["totalFiles"], 16)
            self.assertEqual(saved["verifiedFiles"], 16)
            self.assertEqual(saved["completedBytes"], 1000)
            self.assertEqual(saved["totalBytes"], 1000)
            self.assertEqual(saved["fileIndex"], 16)
            self.assertEqual(saved["currentFile"], "")
            self.assertEqual(saved["currentBytes"], 0)
            self.assertEqual(saved["fileBytes"], 0)
            self.assertEqual(saved["speedBytes"], 0)
            self.assertIsNone(saved["etaSeconds"])

    def test_explicit_download_totals_are_not_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "progress.json").write_text(json.dumps({"phase": "dependencies", "totalFiles": 0, "totalBytes": 0}), encoding="utf-8")

            reporter = ProgressReporter(root, total_files=2, total_bytes=300)
            reporter.set(phase="downloading")

            saved = json.loads((root / "progress.json").read_text(encoding="utf-8"))
            self.assertEqual(saved["totalFiles"], 2)
            self.assertEqual(saved["totalBytes"], 300)


class SupervisorStopAndHotUpgradeTests(unittest.TestCase):
    def test_supervisor_ignores_stale_stop_and_exits_on_new_stop_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            shutil.copy(TEMPLATES / "studio_progress.py", root / "studio_progress.py")
            shutil.copy(TEMPLATES / "toybox_supervisor.py", root / "supervisor.py")
            lock_data = json.loads((TEMPLATES / "toybox-lock.json").read_text(encoding="utf-8"))
            (root / "lock.json").write_text(json.dumps(lock_data), encoding="utf-8")
            (root / "deployment.json").write_text(json.dumps({"id": "owned", "binding": "b1"}), encoding="utf-8")
            (root / "command.json").write_text(json.dumps({"action": "stop_supervisor", "nonce": "stale-nonce"}), encoding="utf-8")

            proc = subprocess.Popen([sys.executable, "-u", str(root / "supervisor.py")], cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            try:
                deadline = time.time() + 12
                state_path = root / "state.json"
                while time.time() < deadline and not state_path.exists():
                    self.assertIsNone(proc.poll(), "supervisor exited prematurely on stale stop_supervisor")
                    time.sleep(0.1)
                self.assertTrue(state_path.exists(), "supervisor never wrote state.json")
                self.assertIsNone(proc.poll(), "supervisor should stay running after ignoring stale stop_supervisor")

                with open(root / "supervisor.lock", "a") as lf:
                    with self.assertRaises(BlockingIOError):
                        fcntl.flock(lf, fcntl.LOCK_EX | fcntl.LOCK_NB)

                tmp_cmd = root / "command.tmp"
                tmp_cmd.write_text(json.dumps({"action": "stop_supervisor", "nonce": "fresh-stop"}), encoding="utf-8")
                tmp_cmd.replace(root / "command.json")

                rc = proc.wait(timeout=12)
                self.assertEqual(rc, 0)

                with open(root / "supervisor.lock", "a") as lf:
                    fcntl.flock(lf, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    fcntl.flock(lf, fcntl.LOCK_UN)
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()
                if proc.stdout:
                    proc.stdout.close()

    def test_hot_upgrade_updates_toybox_only_and_keeps_comfyui_and_tunnel(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp).resolve()
            (root / "ComfyUI").mkdir(parents=True, exist_ok=True)
            shutil.copy(TEMPLATES / "studio_progress.py", root / "studio_progress.py")
            shutil.copy(TEMPLATES / "toybox_supervisor.py", root / "supervisor.py")
            lock_data = json.loads((TEMPLATES / "toybox-lock.json").read_text(encoding="utf-8"))
            (root / "lock.json").write_text(json.dumps(lock_data), encoding="utf-8")
            (root / "deployment.json").write_text(json.dumps({"id": "owned", "binding": "b1"}), encoding="utf-8")
            (root / "installed.json").write_text(json.dumps({"version": "efa7310b54faaf06c4b22a6fd7b18e1c9bfefd69"}), encoding="utf-8")
            cf = root / "cloudflared"
            cf.write_bytes(b"fake-cloudflared")
            lock_data["cloudflared"]["sha256"] = hashlib.sha256(b"fake-cloudflared").hexdigest()
            (root / "lock.json").write_text(json.dumps(lock_data), encoding="utf-8")

            spec = importlib.util.spec_from_file_location("sup_upgrade_test", root / "supervisor.py")
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)

            class FakeProc:
                def __init__(self, alive=True):
                    self.alive = alive
                    self.pid = 999999
                    self.terminated = False

                def poll(self):
                    return None if self.alive else 0

                def terminate(self):
                    self.terminated = True
                    self.alive = False

            comfy_proc = FakeProc(alive=True)
            toybox_proc = FakeProc(alive=True)
            tunnel_proc = FakeProc(alive=True)
            mod.PROCS["comfyui"] = comfy_proc
            mod.PROCS["toybox"] = toybox_proc
            mod.PROCS["tunnel"] = tunnel_proc

            cloned = []
            spawned = []
            ran = []

            def fake_repo_matches(dest, commit):
                return "minimax-h3-colab" not in str(dest)

            mod.repo_matches = fake_repo_matches
            mod.clone = lambda repo, dest: cloned.append((repo, str(dest)))
            mod.run = lambda args, cwd=None: ran.append(args)
            mod.wait_http = lambda url: None
            mod.persist_processes = lambda: None

            def fake_spawn(name, args, cwd=None, env=None):
                spawned.append(name)
                if name in mod.PROCS and mod.PROCS[name].poll() is None:
                    return
                mod.PROCS[name] = FakeProc(alive=True)

            mod.spawn = fake_spawn
            mod.deploy()

            self.assertEqual(ran, [])
            self.assertEqual(cloned, [("shaoyun/minimax-h3-colab", str(root / "minimax-h3-colab"))])
            self.assertFalse(comfy_proc.terminated, "comfyui should stay running when unchanged")
            self.assertFalse(tunnel_proc.terminated, "tunnel should stay running when cloudflared sha matches")
            self.assertTrue(toybox_proc.terminated, "toybox web process must restart on hot upgrade")
            self.assertEqual(spawned, ["comfyui", "toybox", "tunnel"])
            self.assertEqual(json.loads((root / "installed.json").read_text())["version"], lock_data["toybox"])
            self.assertEqual(mod.STATE["stage"], "online")


if __name__ == "__main__":
    unittest.main(verbosity=2)
