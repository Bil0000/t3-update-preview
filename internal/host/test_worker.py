import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("worker", Path(__file__).with_name("worker.py"))
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)
OLD = "0.0.41-preview.20260916.1700"
NEW = "0.0.41-preview.20260916.1794"


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name).resolve()
        self.base = self.home / ".t3"
        self.base.mkdir()
        self.cli = self.home / "bin/t3"
        self.cli.parent.mkdir()
        self.cli.write_bytes(b"\x7fELFold")
        self.cli.chmod(0o700)
        for target, value in (("pathlib.Path.home", self.home), ("platform.system", "Linux"), ("platform.machine", "x86_64"), ("platform.node", "fixture-host")):
            mock = patch(target, return_value=value)
            mock.start()
            self.addCleanup(mock.stop)
        self.network = patch.object(worker.urllib.request, "urlopen", side_effect=AssertionError("Unmocked network is forbidden"))
        self.network.start()
        self.addCleanup(self.network.stop)
        for target in ("build_opener",):
            stub = patch.object(worker.urllib.request, target, side_effect=AssertionError("Unmocked network is forbidden"))
            stub.start()
            self.addCleanup(stub.stop)
        for module, target in ((worker.os, "kill"), (worker.subprocess, "Popen")):
            stub = patch.object(module, target, side_effect=AssertionError("Unmocked process action is forbidden"))
            stub.start()
            self.addCleanup(stub.stop)
        self.process = patch.object(worker.subprocess, "run", side_effect=AssertionError("Unmocked subprocess is forbidden"))
        self.process.start()
        self.addCleanup(self.process.stop)
        for name, value in (("system_identity", "fixture-system-identity"), ("descriptor", {"environmentId": "fixture-environment", "serverVersion": OLD})):
            stub = patch.object(worker.Worker, name, return_value=value)
            stub.start()
            self.addCleanup(stub.stop)
        self.run = patch.object(worker, "run", side_effect=self.fake_run)
        self.commands = self.run.start()
        self.addCleanup(self.run.stop)
        self.request = {"action": "probe", "base_dir": str(self.base), "cli_path": str(self.cli), "target": NEW, "operation": "test-op"}
        self.unit_running = False

    def fake_run(self, args, **kwargs):
        args = [str(a) for a in args]
        if args[-1:] == ["--version"]:
            version = NEW if NEW in Path(args[0]).resolve().parts or "stage" in args[0] else OLD
            return subprocess.CompletedProcess(args, 0, version + "\n", "")
        if args == ["systemctl", "--user", "is-active", "t3code.service"]:
            return subprocess.CompletedProcess(args, 0 if self.unit_running else 3, "active" if self.unit_running else "inactive", "")
        if args == ["systemctl", "--user", "show", "--property=Environment", "--property=ExecStart", "t3code.service"]:
            return subprocess.CompletedProcess(args, 0, "Environment=T3CODE_HOME=" + str(self.base) + "\nExecStart={ path=" + str(self.base / "runtime/versions" / OLD / "t3") + " ; argv[]=t3 __service-launcher ; }\n", "")
        if args == ["ps", "-axo", "pid=,ppid=,command="]:
            return subprocess.CompletedProcess(args, 0, str(os.getpid()) + " 1 python3 test_worker.py\n", "")
        raise AssertionError("Unspecified fixture command: " + repr(args))

    def instance(self, **changes):
        return worker.Worker(dict(self.request, **changes))

    def native_launcher(self):
        runtime = self.base / "runtime/versions" / OLD
        runtime.mkdir(parents=True)
        shutil.copy2(self.cli, runtime / "t3")
        (runtime / ".install-complete").write_text(OLD)
        (runtime / "client").mkdir()
        (runtime / "client/index.html").write_text("old client")
        self.cli.unlink()
        self.cli.symlink_to(os.path.relpath(runtime / "t3", self.cli.parent))
        return runtime

    def database(self):
        userdata = self.base / "userdata"
        userdata.mkdir(exist_ok=True)
        path = userdata / "statev2.sqlite"
        db = sqlite3.connect(path)
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("CREATE TABLE messages (id INTEGER PRIMARY KEY, text TEXT)")
        db.execute("INSERT INTO messages VALUES (1, 'keep this')")
        db.commit()
        return path, db

    def service(self):
        unit = self.home / ".config/systemd/user/t3code.service"
        unit.parent.mkdir(parents=True)
        unit.write_text("[Service]\nExecStart=" + str(self.base / "runtime/versions" / OLD / "t3") + " __service-launcher\nEnvironment=T3CODE_HOME=" + str(self.base) + "\nEnvironment=KEEP=1\n")
        return unit

    def staged(self, service=False):
        if service:
            self.service()
            path, db = self.database()
            db.close()
            userdata = self.base / "userdata"
            (userdata / "server-runtime.json").write_text(json.dumps({"origin": "http://127.0.0.1:4444"}))
        obj = self.instance(action="apply")
        obj.state_dir()
        directory = obj.operation_dir()
        directory.mkdir(mode=0o700)
        (directory / "owner").write_text(worker.MARKER)
        stage = directory / "stage/cli"
        stage.mkdir(parents=True)
        executable = stage / "t3"
        executable.write_bytes(b"\x7fELFnew")
        executable.chmod(0o700)
        obj.journal = {"operation": "test-op", "target": NEW, "status": "staged", "machine_id": obj.machine_id(), "info": obj.probe(), "required_bytes": 1, "cli": "stage/cli/t3", "app": "", "hashes": {"cli/t3": worker.digest(executable)}}
        obj.set_status("staged")
        return obj

    def test_probe_cli_only_has_no_mutations(self):
        info = self.instance().execute()
        self.assertTrue(info["supported"], info)
        self.assertEqual(info["busy"], "idle")
        self.assertFalse((self.base / "update-preview").exists())

    def test_wrong_identity_blocks(self):
        info = self.instance(expected_machine_id="wrong").execute()
        self.assertFalse(info["supported"])
        self.assertIn("identity changed", info["blocker"])

    def test_script_cli_blocks_before_execution(self):
        self.cli.write_text("#!/bin/sh\n")
        self.assertIn("Script/npm", self.instance().probe()["blocker"])
        self.commands.assert_not_called()

    def test_wrong_service_base_blocks(self):
        self.service().write_text("ExecStart=/other/runtime/t3 __service-launcher")
        self.assertIn("another", self.instance().probe()["blocker"])

    def test_health_rejects_external_and_credentials(self):
        for url in ("http://user:pass@localhost:4444", "http://example.com", "https://127.0.0.1/?secret=x"):
            with self.subTest(url=url), self.assertRaises(worker.Blocked):
                self.instance(health_url=url).health_url()

    def test_wal_snapshot_preserves_rows_and_source_settings(self):
        path, db = self.database()
        self.addCleanup(db.close)
        db.execute("INSERT INTO messages VALUES (2, 'from WAL')")
        db.commit()
        settings = path.parent / "settings.json"
        settings.write_text('{"keep": true}')
        target = self.home / "backup.sqlite"
        summary = worker.snapshot_db(path, target)
        self.assertEqual(summary["counts"]["messages"], 2)
        self.assertEqual(settings.read_text(), '{"keep": true}')
        self.assertEqual(worker.db_summary(path), worker.db_summary(target))

    def test_busy_query_and_unknown_terminal_work(self):
        path, db = self.database()
        self.addCleanup(db.close)
        db.execute("CREATE TABLE orchestration_v2_projection_runs (state TEXT)")
        db.execute("INSERT INTO orchestration_v2_projection_runs VALUES ('running')")
        db.commit()
        self.assertEqual(self.instance().busy(True), "busy")
        db.execute("DELETE FROM orchestration_v2_projection_runs")
        db.commit()
        self.assertEqual(self.instance().busy(True), "unknown")

    def test_corrupt_database_fails(self):
        path = self.home / "bad.sqlite"
        path.write_bytes(b"bad database")
        with self.assertRaises(sqlite3.DatabaseError):
            worker.db_summary(path)

    def test_archive_rejects_traversal_links_and_devices(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE), ("link", tarfile.SYMTYPE), ("hard", tarfile.LNKTYPE), ("dev", tarfile.CHRTYPE)):
            with self.subTest(name=name):
                archive = self.home / "unsafe.tar.gz"
                with tarfile.open(archive, "w:gz") as stream:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    member.linkname = "../../escape"
                    stream.addfile(member)
                with self.assertRaises(worker.Blocked):
                    worker.extract_archive(archive, self.home / "extract", 1024)
        self.assertFalse((self.home / "escape").exists())

    def test_archive_budget_and_executable_mode(self):
        archive = self.home / "safe.tar.gz"
        with tarfile.open(archive, "w:gz") as stream:
            member = tarfile.TarInfo("bin/t3")
            member.size = 4
            member.mode = 0o7777
            stream.addfile(member, io.BytesIO(b"test"))
        with self.assertRaises(worker.Blocked):
            worker.extract_archive(archive, self.home / "extract", 3)
        worker.extract_archive(archive, self.home / "extract", 4)
        self.assertEqual((self.home / "extract/bin/t3").stat().st_mode & 0o7777, 0o755)

    def test_checksum_and_size_reject(self):
        asset = {"name": "test.tar.gz", "url": "https://github.com/pingdotgg/t3code/releases/download/v" + NEW + "/test.tar.gz", "digest": "sha256:" + "0" * 64, "size": 4}
        response = io.BytesIO(b"data")
        response.url = asset["url"]
        opener = unittest.mock.Mock()
        opener.open.return_value = response
        with patch.object(worker.urllib.request, "build_opener", return_value=opener), self.assertRaisesRegex(worker.Blocked, "checksum"):
            worker.download(asset, self.home / "download", NEW)

    def test_download_rejects_wrong_origin_before_network(self):
        asset = {"url": "http://github.com/pingdotgg/t3code/releases/download/v" + NEW + "/a", "name": "a"}
        with self.assertRaises(worker.Blocked):
            worker.download(asset, self.home / "download", NEW)
        self.assertFalse((self.home / "download").exists())

    def test_owned_symlink_and_unmarked_directory_block(self):
        link = self.home / "link"
        link.symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(worker.Blocked):
            self.instance(base_dir=str(link))
        (self.base / "update-preview").mkdir()
        with self.assertRaises(worker.Blocked):
            self.instance().state_dir()

    def test_lock_excludes_other_coordinator(self):
        with self.instance().lock(), self.assertRaises(worker.Blocked):
            with self.instance().lock():
                self.fail("second lock acquired")

    def test_operation_traversal_blocked(self):
        self.instance().state_dir()
        with self.assertRaises(worker.Blocked):
            self.instance(operation="../outside").operation_dir()

    def test_apply_cli_only_and_idempotent_retry(self):
        obj = self.staged()
        result = obj.apply()
        self.assertEqual(result["status"], "healthy")
        self.assertTrue(self.cli.is_symlink())
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFnew")
        self.assertFalse((obj.operation_dir() / "stage").exists())
        result = self.instance(action="apply").apply()
        self.assertEqual(result["version"], NEW)
        self.assertEqual(result["status"], "healthy")

    def test_apply_changed_stage_stops_before_backup(self):
        obj = self.staged()
        (obj.operation_dir() / "stage/cli/t3").write_bytes(b"tamper")
        with self.assertRaisesRegex(worker.Blocked, "changed"):
            obj.apply()
        self.assertFalse((obj.operation_dir() / "backup").exists())
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_force_does_not_bypass_space(self):
        obj = self.staged()
        obj.request["force"] = True
        with patch.object(worker.shutil, "disk_usage", return_value=shutil._ntuple_diskusage(1, 1, 0)), self.assertRaisesRegex(worker.Blocked, "Free disk"):
            obj.apply()
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_busy_defers_before_mutation(self):
        obj = self.staged(service=True)
        self.unit_running = True
        result = obj.apply()
        self.assertTrue(result["deferred"])
        self.assertEqual(result["status"], "deferred")
        self.assertFalse((obj.operation_dir() / "backup").exists())

    def test_failed_activation_keeps_backup_and_journal(self):
        obj = self.staged()
        with patch.object(worker.os, "replace", wraps=os.replace) as replace:
            def failure(source, target):
                if Path(target) == self.cli:
                    raise OSError("fixture activation failure")
                return replace._mock_wraps(source, target)
            replace.side_effect = failure
            with self.assertRaises(OSError):
                obj.apply()
        journal = worker.read_json(obj.operation_dir() / "journal.json")
        self.assertEqual(journal["status"], "recovery-required")
        self.assertTrue(Path(journal["backup"]).is_dir())
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_rollback_refuses_new_database_writes(self):
        path, db = self.database()
        obj = self.staged()
        obj.apply()
        db.execute("INSERT INTO messages VALUES (2, 'new work')")
        db.commit()
        db.close()
        with self.assertRaisesRegex(worker.Blocked, "newer work"):
            self.instance(action="rollback").rollback()
        self.assertEqual(worker.db_summary(path)["counts"]["messages"], 2)

    def test_cli_rollback_retains_old_binary(self):
        obj = self.staged()
        obj.apply()
        result = self.instance(action="rollback").rollback()
        self.assertEqual(result["status"], "healthy")
        self.assertEqual(result["recovery"], "rolled-back")
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_cleanup_refuses_recovery_evidence(self):
        obj = self.staged()
        obj.set_status("recovery-required")
        with self.assertRaisesRegex(worker.Blocked, "requires recovery"):
            self.instance(action="cleanup").execute()
        self.assertTrue((obj.operation_dir() / "stage").exists())

    def test_cleanup_never_deletes_unmarked_directory(self):
        obj = self.instance()
        obj.state_dir()
        unowned = obj.root / "unowned"
        unowned.mkdir()
        (unowned / "keep").write_text("keep")
        with self.assertRaises((worker.Blocked, FileNotFoundError)):
            obj.remove_owned(unowned)
        self.assertTrue((unowned / "keep").exists())

    def test_prepare_rejects_bad_version_and_missing_assets(self):
        with self.assertRaises(worker.Blocked):
            self.instance(target="../../bad").prepare()
        with self.assertRaisesRegex(worker.Blocked, "matching standalone"):
            self.instance(assets=[]).prepare()

    def test_main_redacts_unknown_exception(self):
        with patch.object(worker.sys, "stdin", io.StringIO(json.dumps(self.request))), patch.object(worker.sys, "stdout", io.StringIO()) as output, patch.object(worker.Worker, "execute", side_effect=OSError("secret token")):
            self.assertEqual(worker.main(), 1)
        value = json.loads(output.getvalue())
        self.assertNotIn("secret token", value["error"])

    def test_real_cli_version_format(self):
        self.assertEqual(worker.parse_version("t3 v" + NEW + "\n"), NEW)
        self.assertEqual(worker.parse_version("t3 v1.0.0\n"), "")

    def test_service_base_prefix_is_not_a_match(self):
        unit = self.service()
        unit.write_text(unit.read_text().replace(str(self.base), str(self.base) + "-other"))
        with self.assertRaisesRegex(worker.Blocked, "another"):
            self.instance().service_state()

    def test_wrong_descriptor_identity_blocks(self):
        obj = self.staged(service=True)
        (self.base / "userdata/environment-id").write_text("expected-environment")
        self.unit_running = True
        self.assertIn("another T3 environment", obj.probe()["blocker"])

    def test_idle_requires_clear_runs_and_no_child_work(self):
        path, db = self.database()
        self.addCleanup(db.close)
        db.execute("CREATE TABLE orchestration_v2_projection_runs (status TEXT)")
        db.execute("INSERT INTO orchestration_v2_projection_runs VALUES ('completed')")
        db.commit()
        (path.parent / "server-runtime.json").write_text('{"pid": 1234}')
        obj = self.instance()
        with patch.object(obj, "processes", return_value={1234: (1, "t3 server")}):
            self.assertEqual(obj.busy(True), "idle")
        with patch.object(obj, "processes", return_value={1234: (1, "t3 server"), 1235: (1234, "/bin/bash")}):
            self.assertEqual(obj.busy(True), "busy")
        db.execute("UPDATE orchestration_v2_projection_runs SET status='waiting'")
        db.commit()
        self.assertEqual(obj.busy(True), "busy")

    def test_internal_terminal_coordinator_blocks(self):
        obj = self.instance()
        processes = {os.getpid(): (998, "python3"), 998: (1, str(self.base / "runtime/versions" / OLD / "t3") + " server")}
        with patch.object(obj, "processes", return_value=processes), self.assertRaisesRegex(worker.Blocked, "external terminal"):
            obj.require_external_coordinator(None)

    def test_appimage_terminal_coordinator_blocks(self):
        obj = self.instance()
        processes = {os.getpid(): (998, "python3"), 998: (1, "/tmp/.mount_T3/bin/t3")}
        with patch.object(obj, "processes", return_value=processes), patch.object(obj, "appimage_processes", return_value=[998]), self.assertRaisesRegex(worker.Blocked, "external terminal"):
            obj.require_external_coordinator(self.home / "T3.AppImage")

    def test_rollback_refuses_changed_settings_and_new_database(self):
        path, db = self.database()
        db.close()
        settings = path.parent / "settings.json"
        settings.write_text('{"theme":"old"}')
        obj = self.staged()
        obj.apply()
        settings.write_text('{"theme":"new"}')
        with self.assertRaisesRegex(worker.Blocked, "newer work"):
            self.instance(action="rollback").rollback()
        settings.write_text('{"theme":"old"}')
        with sqlite3.connect(path.parent / "new.sqlite") as extra:
            extra.execute("CREATE TABLE work (id INTEGER)")
        with self.assertRaisesRegex(worker.Blocked, "newer work"):
            self.instance(action="rollback").rollback()
        self.assertTrue((path.parent / "new.sqlite").is_file())

    def test_thread_ids_checked_not_just_counts(self):
        path, db = self.database()
        snapshot = self.home / "snapshot.sqlite"
        worker.snapshot_db(path, snapshot)
        db.execute("UPDATE messages SET id=2 WHERE id=1")
        db.commit()
        db.close()
        with self.assertRaisesRegex(worker.Blocked, "identifiers"):
            worker.verify_preserved_rows(path, snapshot)

    def test_first_failed_operation_is_visible_to_status(self):
        obj = self.staged()
        obj.set_status("recovery-required", activation_started=True)
        result = self.instance(action="status", operation="").execute()
        self.assertEqual(result["status"], "recovery-required")
        self.assertEqual(result["operation"], "test-op")

    def test_pre_activation_failure_restores_safe_state(self):
        obj = self.staged()
        with patch.object(obj, "backup", side_effect=OSError("fixture disk failure")), self.assertRaises(OSError):
            obj.apply()
        self.assertEqual(worker.read_json(obj.operation_dir() / "journal.json")["status"], "aborted-safe")
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")
        self.assertEqual(worker.read_json(obj.root / "active.json"), {})

    def test_cleanup_failed_prepare_keeps_latest_rollback(self):
        obj = self.staged()
        obj.apply()
        latest = worker.read_json(obj.root / "latest.json")
        directory = obj.root / "failed-stage"
        directory.mkdir(mode=0o700)
        (directory / "owner").write_text(worker.MARKER)
        worker.write_json(directory / "journal.json", {"status": "prepare-failed"})
        worker.write_json(obj.root / "active.json", {"operation": "failed-stage"})
        self.instance(action="cleanup", operation="failed-stage").execute()
        self.assertFalse(directory.exists())
        self.assertEqual(worker.read_json(obj.root / "active.json"), latest)

    def test_native_activation_keeps_upstream_old_runtimes(self):
        obj = self.staged()
        old = self.base / "runtime/versions" / OLD
        old.mkdir(parents=True)
        (old / "t3").write_bytes(b"\x7fELFold")
        obj.apply()
        self.assertTrue(old.exists())
        self.assertEqual(self.cli.resolve(), self.base / "runtime/versions" / NEW / "t3")
        self.assertEqual(list(obj.root.glob("install-*")), [])

    def test_stopped_backup_state_can_rollback(self):
        obj = self.staged()
        info = obj.probe()
        obj.journal["restore_service_version"] = OLD
        obj.backup(obj.operation_dir(), info)
        result = self.instance(action="rollback").rollback()
        self.assertEqual(result["status"], "healthy")

    def test_appimage_metadata_isolated_without_gui_launch(self):
        app = self.home / "T3.AppImage"
        header = bytearray(20)
        header[:4] = b"\x7fELF"
        header[5] = 1
        header[8:11] = b"AI\x02"
        header[18:20] = (62).to_bytes(2, "little")
        app.write_bytes(header)
        def extraction(args, **kwargs):
            self.assertEqual(args, [str(app), "--appimage-extract", "resources/*"])
            self.assertEqual(kwargs["env"]["HOME"], str(kwargs["cwd"]))
            package = Path(kwargs["cwd"]) / "squashfs-root/resources/app/package.json"
            package.parent.mkdir(parents=True)
            package.write_text(json.dumps({"name": "@t3tools/desktop", "version": NEW}))
            return subprocess.CompletedProcess(args, 0, "", "")
        with patch.object(worker.subprocess, "run", side_effect=extraction):
            self.assertEqual(self.instance().appimage_version(app), NEW)

    def test_appimage_wrong_cpu_rejected_before_execution(self):
        app = self.home / "wrong.AppImage"
        header = bytearray(20)
        header[:4] = b"\x7fELF"
        header[5] = 1
        header[8:11] = b"AI\x02"
        header[18:20] = (183).to_bytes(2, "little")
        app.write_bytes(header)
        with self.assertRaisesRegex(worker.Blocked, "CPU"):
            self.instance().appimage_version(app)

    def test_service_migration_stays_stopped_until_original_environment_restored(self):
        original_runtime = self.native_launcher()
        worker.write_json(self.base / "runtime/service-state.json", {"protocol": 2, "activeVersion": OLD})
        obj = self.staged(service=True)
        old_run = self.fake_run
        def command(args, **kwargs):
            args = [str(a) for a in args]
            if args == ["systemctl", "--user", "daemon-reload"]:
                return subprocess.CompletedProcess(args, 0, "", "")
            if args == ["systemctl", "--user", "stop", "t3code.service"]:
                self.unit_running = False
                obj.runtime.unlink(missing_ok=True)
                return subprocess.CompletedProcess(args, 0, "", "")
            if args in (["systemctl", "--user", "restart", "t3code.service"], ["systemctl", "--user", "start", "t3code.service"]):
                self.assertIn("Environment=KEEP=1", obj.unit.read_text())
                self.unit_running = True
                obj.runtime.write_text(json.dumps({"origin": "http://127.0.0.1:4444", "pid": 1234}))
                return subprocess.CompletedProcess(args, 0, "", "")
            if args == ["ps", "-p", "1234", "-o", "command="]:
                version = worker.read_json(self.base / "runtime/service-state.json")["activeVersion"]
                return subprocess.CompletedProcess(args, 0, str(self.base / "runtime/versions" / version / "t3") + " server", "")
            return old_run(args, **kwargs)
        def update(args, **kwargs):
            self.assertEqual(args[1:], ["update", NEW, "--base-dir", str(self.base)])
            self.assertEqual(kwargs["stdin"], subprocess.DEVNULL)
            self.assertNotIn("T3CODE_RELEASE_BASE_URL", kwargs["env"])
            self.assertFalse(self.unit_running)
            obj.unit.write_text("[Service]\nExecStart=" + str(self.base / "runtime/versions" / NEW / "t3") + " __service-launcher\nEnvironment=T3CODE_HOME=" + str(self.base) + "\n")
            (self.base / "runtime").mkdir(exist_ok=True)
            pinned = self.base / "runtime/versions" / NEW
            self.assertEqual(Path(args[0]), pinned / "t3")
            self.assertEqual((pinned / ".install-complete").read_text(), NEW)
            worker.write_json(self.base / "runtime/service-state.json", {"protocol": 3, "activeVersion": NEW})
            return subprocess.CompletedProcess(args, 0, "", "")
        def descriptor(_):
            return {"serverVersion": worker.read_json(self.base / "runtime/service-state.json")["activeVersion"], "environmentId": "fixture-environment"}
        with patch.object(worker, "run", side_effect=command), patch.object(worker.subprocess, "run", side_effect=update), patch.object(obj, "descriptor", side_effect=descriptor), patch.object(worker.time, "sleep"):
            result = obj.apply()
            self.assertEqual(result["status"], "healthy")
            self.assertEqual(result["runtime_version"], NEW)
            self.assertEqual(result["service_version"], NEW)
            self.assertIn("Environment=KEEP=1", obj.unit.read_text())
            self.assertEqual(self.cli.resolve(), self.base / "runtime/versions" / NEW / "t3")
            obj.request["force"] = True
            result = obj.rollback()
            self.assertEqual(result["runtime_version"], OLD)
            self.assertEqual(self.cli.resolve(), original_runtime / "t3")
            self.assertTrue(self.cli.is_symlink())

    def desktop_fixture(self, operating_system):
        obj = self.staged()
        obj.os = operating_system
        path, db = self.database()
        db.close()
        obj.unit = self.home / "unused-service"
        obj.runtime.write_text(json.dumps({"origin": "http://127.0.0.1:4444"}))
        app = self.home / ("My Custom T3.app" if operating_system == "darwin" else "My-T3-Code.AppImage")
        staged_app = obj.operation_dir() / ("stage/desktop.app" if operating_system == "darwin" else "stage/desktop.AppImage")
        if operating_system == "darwin":
            (app / "Contents").mkdir(parents=True)
            (app / "Contents/Info.plist").write_bytes(worker.plistlib.dumps({"CFBundleIdentifier": "com.t3tools.t3code", "CFBundleShortVersionString": OLD}))
            (app / "Icon\r").write_bytes(b"custom icon")
            shutil.copytree(app, staged_app)
            (staged_app / "Contents/Info.plist").write_bytes(worker.plistlib.dumps({"CFBundleIdentifier": "com.t3tools.t3code", "CFBundleShortVersionString": NEW}))
        else:
            app.write_bytes(b"old AppImage")
            app.chmod(0o750)
            staged_app.write_bytes(b"new AppImage")
            staged_app.chmod(0o750)
        info = dict(obj.journal["info"], app_path=str(app), desktop_version=OLD, desktop_running=True, health_url="http://127.0.0.1:4444", runtime_version=OLD, environment_id="fixture-environment")
        obj.journal["info"] = info
        obj.journal["app"] = str(staged_app.relative_to(obj.operation_dir()))
        stage = obj.operation_dir() / "stage"
        obj.journal["hashes"] = {str(p.relative_to(stage)): worker.digest(p) for p in stage.rglob("*") if p.is_file()}
        obj.set_status("staged")
        return obj, app, info

    def test_mac_embedded_apply_and_rollback_keep_custom_name_and_icon(self):
        obj, app, info = self.desktop_fixture("darwin")
        running = {"value": True}
        def command(args, **kwargs):
            args = [str(a) for a in args]
            if args[0] == "osascript":
                running["value"] = False
                obj.runtime.unlink(missing_ok=True)
                return subprocess.CompletedProcess(args, 0, "", "")
            if args[0] == "ditto":
                worker.copy_path(Path(args[-2]), Path(args[-1]))
                return subprocess.CompletedProcess(args, 0, "", "")
            if args[:2] == ["ps", "-p"]:
                return subprocess.CompletedProcess(args, 0, str(app / "Contents/Resources/server/t3") + " server", "")
            return self.fake_run(args, **kwargs)
        def open_app(path):
            self.assertEqual(path, app)
            running["value"] = True
            obj.runtime.write_text(json.dumps({"origin": info["health_url"], "pid": 1234}))
        def descriptor(_):
            return {"serverVersion": worker.plistlib.loads((app / "Contents/Info.plist").read_bytes())["CFBundleShortVersionString"], "environmentId": "fixture-environment"}
        with patch.object(obj, "probe", return_value=info), patch.object(obj, "desktop_running", side_effect=lambda _: running["value"]), patch.object(obj, "signature", return_value="TEAM"), patch.object(obj, "open_app", side_effect=open_app), patch.object(obj, "descriptor", side_effect=descriptor), patch.object(worker, "run", side_effect=command), patch.object(worker.time, "sleep"):
            result = obj.apply()
            self.assertEqual(result["runtime_version"], NEW)
            self.assertEqual((app / "Icon\r").read_bytes(), b"custom icon")
            self.assertEqual(app.name, "My Custom T3.app")
            obj.request["force"] = True
            result = obj.rollback()
            self.assertEqual(result["runtime_version"], OLD)
            self.assertEqual((app / "Icon\r").read_bytes(), b"custom icon")
            self.assertTrue(running["value"])

    def test_appimage_embedded_apply_and_rollback_use_actual_process_identity(self):
        obj, app, info = self.desktop_fixture("linux")
        running = {"value": True}
        def terminate(pid, sig):
            self.assertEqual((pid, sig), (1234, worker.signal.SIGTERM))
            running["value"] = False
            obj.runtime.unlink(missing_ok=True)
        def command(args, **kwargs):
            args = [str(a) for a in args]
            if args[:2] == ["ps", "-p"]:
                return subprocess.CompletedProcess(args, 0, "/tmp/.mount_T3/resources/server/t3 server", "")
            return self.fake_run(args, **kwargs)
        def open_app(path):
            self.assertEqual(path, app)
            running["value"] = True
            obj.runtime.write_text(json.dumps({"origin": info["health_url"], "pid": 1234}))
        def descriptor(_):
            return {"serverVersion": NEW if app.read_bytes() == b"new AppImage" else OLD, "environmentId": "fixture-environment"}
        with patch.object(obj, "probe", return_value=info), patch.object(obj, "appimage_processes", side_effect=lambda _: [1234] if running["value"] else []), patch.object(obj, "open_app", side_effect=open_app), patch.object(obj, "descriptor", side_effect=descriptor), patch.object(worker, "run", side_effect=command), patch.object(worker.os, "kill", side_effect=terminate), patch.object(worker.time, "sleep"):
            result = obj.apply()
            self.assertEqual(result["runtime_version"], NEW)
            self.assertEqual(app.stat().st_mode & 0o777, 0o750)
            obj.request["force"] = True
            result = obj.rollback()
            self.assertEqual(result["runtime_version"], OLD)
            self.assertEqual(app.read_bytes(), b"old AppImage")
            self.assertTrue(running["value"])

    def test_rollback_preserves_a_symlinked_original_cli(self):
        target = self.home / "original/t3"
        target.parent.mkdir()
        target.write_bytes(self.cli.read_bytes())
        target.chmod(0o700)
        self.cli.unlink()
        self.cli.symlink_to(target)
        obj = self.staged()
        obj.apply()
        obj.rollback()
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_corrupted_recovery_binary_blocks_before_restore(self):
        obj = self.staged()
        obj.apply()
        saved = obj.operation_dir() / "backup/cli-runtime/t3"
        saved.write_bytes(b"corrupted")
        with self.assertRaisesRegex(worker.Blocked, "backup contents changed"):
            obj.rollback()
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFnew")

    def test_unknown_run_state_never_counts_as_idle(self):
        path, db = self.database()
        self.addCleanup(db.close)
        db.execute("CREATE TABLE orchestration_v2_projection_runs (status TEXT)")
        db.execute("INSERT INTO orchestration_v2_projection_runs VALUES ('future-active-state')")
        db.commit()
        self.assertEqual(self.instance().busy(True), "busy")

    def test_health_redirect_is_rejected(self):
        with self.assertRaises(worker.Blocked):
            worker.NoRedirect().redirect_request(None, None, 302, "", {}, "https://example.com/")

    def test_mac_prepare_preserves_finder_metadata_and_detaches_dmg(self):
        obj, app, info = self.desktop_fixture("darwin")
        obj.operation = "prepare-mac"
        obj.arch = "arm64"
        obj.request["assets"] = [{"name": "t3-darwin-arm64.tar.gz", "size": 1}, {"name": "T3-Code-arm64.dmg", "size": 1}]
        archive = self.home / "release.tar.gz"
        with tarfile.open(archive, "w:gz") as stream:
            entry = tarfile.TarInfo("t3")
            entry.size = 7
            entry.mode = 0o755
            stream.addfile(entry, io.BytesIO(b"new-cli"))
        volume = self.home / "mounted"
        packaged = volume / "T3 Code.app"
        (packaged / "Contents").mkdir(parents=True)
        (packaged / "Contents/Info.plist").write_bytes(worker.plistlib.dumps({"CFBundleIdentifier": "com.t3tools.t3code", "CFBundleShortVersionString": NEW}))
        detached = []
        finder_writes = []
        def command(args, **kwargs):
            args = [str(a) for a in args]
            if args[:2] == ["hdiutil", "attach"]:
                return subprocess.CompletedProcess(args, 0, worker.plistlib.dumps({"system-entities": [{"mount-point": str(volume)}]}).decode(), "")
            if args[:2] == ["hdiutil", "detach"]:
                detached.append(args[-1])
            elif args[:2] == ["xattr", "-px"]:
                return subprocess.CompletedProcess(args, 0, "ab" * 32, "")
            elif args[:2] == ["xattr", "-wx"]:
                finder_writes.append(args)
            elif args[0] == "ditto":
                worker.copy_path(Path(args[-2]), Path(args[-1]))
            elif args[0] not in ("hdiutil", "spctl"):
                raise AssertionError(args)
            return subprocess.CompletedProcess(args, 0, "", "")
        def download(asset, destination, target):
            if asset["name"].endswith(".tar.gz"):
                shutil.copy2(archive, destination)
            else:
                destination.write_bytes(b"dmg")
        with patch.object(obj, "probe", return_value=info), patch.object(obj, "signature", return_value="TEAM"), patch.object(worker, "download", side_effect=download), patch.object(worker, "run", side_effect=command), patch.object(worker.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "t3 v" + NEW, "")):
            result = obj.prepare()
        self.assertEqual(result["status"], "staged")
        self.assertEqual(detached, [str(volume)])
        self.assertEqual((obj.operation_dir() / "stage/desktop.app/Icon\r").read_bytes(), b"custom icon")
        self.assertEqual(finder_writes, [["xattr", "-wx", "com.apple.FinderInfo", "ab" * 32, str(obj.operation_dir() / "stage/desktop.app")]])

    def test_failed_dmg_detach_retains_only_failed_mount_for_retry(self):
        obj = self.staged()
        obj.set_status("prepare-failed", mounts=["/fixture/first", "/fixture/second"])
        attempted = []
        def detach(args, **kwargs):
            attempted.append(args[-1])
            if args[-1] == "/fixture/first":
                raise worker.Blocked("fixture busy mount")
            return subprocess.CompletedProcess(args, 0, "", "")
        with patch.object(worker, "run", side_effect=detach), self.assertRaisesRegex(worker.Blocked, "retained"):
            obj.detach_mounts()
        self.assertEqual(attempted, ["/fixture/first", "/fixture/second"])
        self.assertEqual(obj.journal["mounts"], ["/fixture/first"])
        self.assertTrue((obj.operation_dir() / "stage").exists())

    def test_existing_target_runtime_must_match_verified_archive(self):
        obj = self.staged()
        target = self.base / "runtime/versions" / NEW
        target.mkdir(parents=True)
        (target / "t3").write_bytes(b"untrusted-existing-binary")
        with self.assertRaisesRegex(worker.Blocked, "verified official archive"):
            obj.verify_runtime_artifacts(obj.operation_dir() / "stage/cli/t3", required=False)

    def test_writes_during_health_verification_block_rollback(self):
        path, db = self.database()
        db.close()
        obj = self.staged()
        original_verify = obj.verify
        def verify(info, version):
            with sqlite3.connect(path) as live:
                live.execute("INSERT INTO messages VALUES (2, 'written during health check')")
            return original_verify(info, version)
        with patch.object(obj, "verify", side_effect=verify):
            obj.apply()
        with self.assertRaisesRegex(worker.Blocked, "newer work"):
            obj.rollback()
        self.assertEqual(worker.db_summary(path)["counts"]["messages"], 2)

    def test_rollback_refuses_changed_service_settings(self):
        obj = self.staged(service=True)
        obj.journal["restore_service_version"] = OLD
        obj.backup(obj.operation_dir(), obj.probe())
        obj.unit.write_text(obj.unit.read_text() + "Environment=NEW_USER_SETTING=1\n")
        with self.assertRaisesRegex(worker.Blocked, "newer configuration"):
            obj.rollback()
        self.assertIn("NEW_USER_SETTING", obj.unit.read_text())

    def test_rollback_refuses_new_service_dropin(self):
        obj = self.staged(service=True)
        obj.journal["restore_service_version"] = OLD
        obj.backup(obj.operation_dir(), obj.probe())
        dropins = Path(str(obj.unit) + ".d")
        dropins.mkdir()
        (dropins / "override.conf").write_text("[Service]\nEnvironment=KEEP_NEW=1\n")
        with self.assertRaisesRegex(worker.Blocked, "newer configuration"):
            obj.rollback()
        self.assertTrue((dropins / "override.conf").is_file())

    def test_cross_filesystem_launcher_blocks_before_stopping(self):
        obj = self.staged()
        original = Path.stat
        def stat(path, *args, **kwargs):
            result = original(path, *args, **kwargs)
            if path == self.cli.parent:
                return unittest.mock.Mock(st_dev=result.st_dev + 1, st_uid=result.st_uid, st_mode=result.st_mode)
            return result
        with patch.object(Path, "stat", stat), patch.object(obj, "stop") as stop, self.assertRaisesRegex(worker.Blocked, "filesystems"):
            obj.apply()
        stop.assert_not_called()

    def test_untrusted_release_redirect_is_rejected_before_contact(self):
        with self.assertRaises(worker.Blocked):
            worker.ReleaseRedirect().redirect_request(None, None, 302, "", {}, "https://attacker.example/asset")

    def test_cli_only_apply_and_rollback_preserve_native_launcher_layout(self):
        old = self.native_launcher()
        original_link = os.readlink(self.cli)
        obj = self.staged()
        obj.apply()
        current = self.base / "runtime/versions" / NEW / "t3"
        self.assertEqual(self.cli.resolve(), current)
        self.assertEqual((current.parent / ".install-complete").read_text(), NEW)
        obj.rollback()
        self.assertEqual(os.readlink(self.cli), original_link)
        self.assertEqual(self.cli.resolve(), old / "t3")
        self.assertTrue(current.exists())
        self.assertEqual(list(obj.root.glob("install-*")), [])

    def test_missing_original_native_runtime_restored_before_launcher_start(self):
        old = self.native_launcher()
        obj = self.staged()
        obj.apply()
        shutil.rmtree(old)
        result = obj.rollback()
        self.assertEqual(result["status"], "healthy")
        self.assertEqual(self.cli.resolve(), old / "t3")
        self.assertEqual((old / "client/index.html").read_text(), "old client")
        self.assertEqual((old / ".install-complete").read_text(), OLD)

    def test_missing_unknown_original_launcher_target_blocks_before_stop(self):
        target = self.home / "custom/t3"
        target.parent.mkdir()
        shutil.copy2(self.cli, target)
        self.cli.unlink()
        self.cli.symlink_to(target)
        obj = self.staged()
        obj.apply()
        target.unlink()
        with patch.object(obj, "stop") as stop, self.assertRaisesRegex(worker.Blocked, "outside a recoverable native runtime"):
            obj.rollback()
        stop.assert_not_called()
        self.assertEqual(self.cli.resolve(), self.base / "runtime/versions" / NEW / "t3")

    def test_original_regular_launcher_restored_as_regular_file(self):
        obj = self.staged()
        obj.apply()
        obj.rollback()
        self.assertFalse(self.cli.is_symlink())
        self.assertEqual(self.cli.read_bytes(), b"\x7fELFold")

    def test_direct_native_runtime_path_blocks_before_mutation(self):
        old = self.native_launcher()
        obj = self.instance(cli_path=str(old / "t3"))
        info = obj.probe()
        self.assertFalse(info["supported"])
        self.assertIn("external CLI launcher", info["blocker"])
        self.assertFalse((old / "t3").is_symlink())
        self.assertEqual((old / "t3").read_bytes(), b"\x7fELFold")


if __name__ == "__main__":
    unittest.main()
