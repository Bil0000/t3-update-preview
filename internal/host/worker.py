import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import platform
import plistlib
import re
import shlex
import shutil
import signal
import sqlite3
import ssl
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

Path = pathlib.Path
VERSION = re.compile(r"^\d+\.\d+\.\d+-preview\.[0-9.]+$")
OPERATION = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$")
MARKER = "t3-update-preview-v1"


class Blocked(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise Blocked("HTTP redirects are not allowed during host health checks")


class ReleaseRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlparse(newurl)
        if target.scheme != "https" or target.hostname not in ("github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com") or target.username or target.password or target.port not in (None, 443):
            raise Blocked("Untrusted release redirect")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def open_url(url, redirect, timeout):
    context = ssl.create_default_context()
    if platform.system() == "Darwin" and "SSL_CERT_FILE" not in os.environ and "SSL_CERT_DIR" not in os.environ:
        paths = ssl.get_default_verify_paths()
        if not paths.cafile and not paths.capath:
            context.load_verify_locations("/etc/ssl/cert.pem")
    try:
        return urllib.request.build_opener(redirect, urllib.request.HTTPSHandler(context=context)).open(url, timeout=timeout)
    except urllib.error.URLError as error:
        if isinstance(error.reason, ssl.SSLCertVerificationError):
            raise Blocked("TLS certificate verification failed; check this host's trusted CA certificates") from None
        raise


def parse_version(text):
    value = text.strip()
    match = re.search(r"(?:^|\s)v?(\d+\.\d+\.\d+-preview\.[0-9.]+)$", value)
    return match.group(1) if match else ""


def run(args, check=True, timeout=90):
    result = subprocess.run([str(v) for v in args], capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        raise Blocked("Command failed: " + Path(str(args[0])).name + " (exit " + str(result.returncode) + ")")
    return result


def read_json(path, default=None):
    if not path.exists():
        return default
    with path.open() as stream:
        return json.load(stream)


def write_json(path, value):
    fd, name = tempfile.mkstemp(prefix=".write-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def owned(path, links=False):
    path = Path(path).absolute()
    for item in [path, *path.parents]:
        if item.is_symlink() and not (links and item == path):
            raise Blocked("Symlink path is not supported: " + str(item))
    if path.exists() and path.stat().st_uid != os.getuid():
        raise Blocked("Installation is not owned by this user: " + str(path))
    return path


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def db_summary(path):
    with contextlib.closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=10)) as db:
        if db.execute("PRAGMA quick_check").fetchall() != [("ok",)]:
            raise Blocked("Database integrity check failed: " + path.name)
        tables = [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        counts = {}
        value = hashlib.sha256()
        for table in tables:
            quoted = '"' + table.replace('"', '""') + '"'
            counts[table] = db.execute("SELECT COUNT(*) FROM " + quoted).fetchone()[0]
        for line in db.iterdump():
            value.update(line.encode())
            value.update(b"\n")
        return {"counts": counts, "fingerprint": value.hexdigest()}


def snapshot_db(source, target):
    before = db_summary(source)
    with contextlib.closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True, timeout=10)) as db:
        db.execute("VACUUM INTO ?", (str(target),))
    os.chmod(target, 0o600)
    after = db_summary(target)
    if before != after:
        raise Blocked("Database changed during stopped backup: " + source.name)
    return after


def verify_preserved_rows(source, snapshot):
    with contextlib.closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True, timeout=10)) as db:
        db.execute("ATTACH DATABASE ? AS snapshot", (snapshot.as_uri() + "?mode=ro",))
        tables = [row[0] for row in db.execute("SELECT name FROM snapshot.sqlite_master WHERE type='table'")]
        for table in tables:
            if not any(name in table for name in ("threads", "messages", "events")):
                continue
            quoted = '"' + table.replace('"', '""') + '"'
            columns = [row[1] for row in db.execute("PRAGMA snapshot.table_info(" + quoted + ")") if row[5]]
            if not columns:
                continue
            keys = ", ".join('"' + column.replace('"', '""') + '"' for column in columns)
            missing = db.execute("SELECT " + keys + " FROM snapshot." + quoted + " EXCEPT SELECT " + keys + " FROM main." + quoted + " LIMIT 1").fetchone()
            if missing:
                raise Blocked("Existing thread, message, or event identifiers are missing after startup")


def copy_path(source, target):
    if source.is_symlink():
        target.symlink_to(os.readlink(source))
    elif source.is_dir():
        shutil.copytree(source, target, symlinks=True)
    else:
        shutil.copy2(source, target)


def tree_size(path):
    if path.is_symlink():
        return 0
    if path.is_file():
        return path.stat().st_size
    if path.is_dir():
        return sum(tree_size(child) for child in path.iterdir())
    return 0


def data_inventory(path):
    result = {}
    if not path.exists() and not path.is_symlink():
        return {".": "missing"}
    entries = [path] if not path.is_dir() else [path, *path.rglob("*")]
    for item in entries:
        relative = item.relative_to(path) if item != path else Path(".")
        if "logs" in relative.parts or item.name == "server-runtime.json" or item.name.endswith(("-wal", "-shm")):
            continue
        if item.is_symlink():
            value = "link:" + os.readlink(item)
        elif item.is_dir():
            value = "directory"
        elif item.suffix in (".sqlite", ".sqlite3", ".db"):
            value = "sqlite:" + db_summary(item)["fingerprint"]
        else:
            value = "file:" + digest(item)
        result[str(relative)] = value
    return result


def file_manifest(directory):
    return {str(path.relative_to(directory)): "link:" + os.readlink(path) if path.is_symlink() else digest(path) for path in directory.rglob("*") if path.is_file() or path.is_symlink()}


def extract_archive(archive, destination, budget):
    with tarfile.open(archive, "r:gz") as stream:
        members = stream.getmembers()
        if sum(max(0, m.size) for m in members) > budget:
            raise Blocked("Archive exceeds available staging space")
        for member in members:
            name = pathlib.PurePosixPath(member.name)
            if name.is_absolute() or ".." in name.parts or member.issym() or member.islnk() or not (member.isfile() or member.isdir()):
                raise Blocked("Archive contains an unsafe path or link")
        for member in members:
            target = destination.joinpath(*pathlib.PurePosixPath(member.name).parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with stream.extractfile(member) as source, target.open("xb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o600)


def download(asset, destination, target):
    asset_url = asset.get("url") or asset.get("browser_download_url", "")
    url = urllib.parse.urlparse(asset_url)
    prefix = "/pingdotgg/t3code/releases/download/v" + target + "/"
    if url.scheme != "https" or url.netloc != "github.com" or not url.path.startswith(prefix) or url.query or url.fragment:
        raise Blocked("Asset must come from the exact official GitHub preview release")
    name = asset.get("name", "")
    if Path(name).name != name or not name or urllib.parse.unquote(url.path.rsplit("/", 1)[-1]) != name:
        raise Blocked("Invalid asset name")
    checksum = asset.get("digest", "")
    if not re.fullmatch(r"sha256:[a-fA-F0-9]{64}", checksum):
        raise Blocked("Asset requires a trusted SHA-256 digest")
    size = asset.get("size")
    if not isinstance(size, int) or isinstance(size, bool) or size <= 0:
        raise Blocked("Asset requires a positive verified size")
    with open_url(asset_url, ReleaseRedirect, 60) as response, destination.open("xb") as stream:
        final = urllib.parse.urlparse(response.url)
        if final.scheme != "https" or final.hostname not in ("github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"):
            raise Blocked("Untrusted asset redirect")
        written = 0
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            written += len(chunk)
            if written > size:
                raise Blocked("Asset is larger than its release metadata")
            stream.write(chunk)
    if written != size or digest(destination) != checksum[7:].lower():
        raise Blocked("Asset checksum or size mismatch")


class Worker:
    def __init__(self, request):
        if not isinstance(request, dict):
            raise Blocked("Request must be a JSON object")
        if "force" in request and not isinstance(request["force"], bool):
            raise Blocked("force must be a JSON boolean")
        self.request = request
        self.home = Path.home()
        self.base = owned(Path(request.get("base_dir") or self.home / ".t3").expanduser())
        if not self.base.is_dir() or self.base == self.home or self.base == Path("/"):
            raise Blocked("base_dir must name an existing dedicated T3 data directory")
        self.root = self.base / "update-preview"
        self.os = platform.system().lower()
        self.arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "x64", "amd64": "x64"}.get(platform.machine().lower(), "unknown")
        self.operation = request.get("operation", "")
        self.target = request.get("target", "")
        self.unit = self.home / ("Library/LaunchAgents/com.t3tools.t3code.service.plist" if self.os == "darwin" else ".config/systemd/user/t3code.service")
        self.runtime = self.base / "userdata/server-runtime.json"
        self.journal = None

    def machine_id(self):
        return hashlib.sha256((self.system_identity() + ":" + str(os.getuid()) + ":" + str(self.base)).encode()).hexdigest()

    def system_identity(self):
        if self.os == "linux":
            path = Path("/etc/machine-id")
            if path.is_file() and re.fullmatch(r"[a-fA-F0-9]{32}", path.read_text().strip()):
                return path.read_text().strip()
        if self.os == "darwin":
            result = run(["ioreg", "-rd1", "-c", "IOPlatformExpertDevice"])
            match = re.search(r'"IOPlatformUUID"\s*=\s*"([A-Fa-f0-9-]+)"', result.stdout)
            if match:
                return match.group(1)
        raise Blocked("No stable operating system machine identity is available")

    def environment_id(self):
        identity = self.base / "userdata/environment-id"
        if identity.is_file():
            value = identity.read_text().strip()
            if re.fullmatch(r"[a-zA-Z0-9_-]{8,128}", value):
                return value
        return ""

    def state_dir(self):
        owned(self.root)
        if self.root.exists():
            if not self.root.is_dir() or not (self.root / "owner").is_file() or (self.root / "owner").read_text() != MARKER:
                raise Blocked("Updater directory lacks its ownership marker")
            if self.root.stat().st_mode & 0o077:
                raise Blocked("Updater directory must have private permissions (0700)")
        else:
            self.root.mkdir(mode=0o700)
            (self.root / "owner").write_text(MARKER)
        return self.root

    @contextlib.contextmanager
    def lock(self):
        self.state_dir()
        lock_path = owned(self.root / "lock")
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            try:
                fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Blocked("Another updater holds this device lock") from None
            yield

    def remove_owned(self, path):
        owned(path)
        if path.parent != self.root or not OPERATION.fullmatch(path.name) or not path.is_dir() or (path / "owner").read_text() != MARKER:
            raise Blocked("Refusing to remove a directory without an updater ownership marker")
        shutil.rmtree(path)

    def cli(self):
        selected = self.request.get("cli_path") or shutil.which("t3")
        if not selected and (self.home / ".local/bin/t3").exists():
            selected = str(self.home / ".local/bin/t3")
        return Path(selected).expanduser().absolute() if selected else None

    def app(self):
        if self.request.get("app_path"):
            return Path(self.request["app_path"]).expanduser().absolute()
        if self.os == "darwin":
            candidates = []
            for directory in (self.home / "Applications", Path("/Applications")):
                if directory.is_dir():
                    for app in directory.glob("*.app"):
                        try:
                            info = plistlib.loads((app / "Contents/Info.plist").read_bytes())
                            if info.get("CFBundleIdentifier") == "com.t3tools.t3code" and "preview" in info.get("CFBundleShortVersionString", ""):
                                candidates.append(app)
                        except (OSError, ValueError):
                            continue
            if len(candidates) > 1:
                raise Blocked("Multiple preview apps found; enroll an explicit app_path")
            return candidates[0] if candidates else None
        if self.os == "linux":
            candidates = []
            for directory in (self.home / "Applications", self.home / ".local/bin"):
                if directory.is_dir():
                    for app in directory.glob("T3*Code*.AppImage"):
                        if VERSION.fullmatch(self.appimage_version(app)):
                            candidates.append(app)
            if len(candidates) > 1:
                raise Blocked("Multiple preview AppImages found; enroll an explicit app_path")
            return candidates[0] if candidates else None
        return None

    def service_state(self):
        if not self.unit.exists():
            return False, False
        owned(self.unit)
        text = self.unit.read_text()
        if self.os == "darwin":
            data = plistlib.loads(self.unit.read_bytes())
            args = data.get("ProgramArguments", [])
            env = data.get("EnvironmentVariables", {})
            matched = bool(args) and args[0].startswith(str(self.base / "runtime") + "/") and "__service-launcher" in args and env.get("T3CODE_HOME") == str(self.base)
        else:
            commands = re.findall(r"^ExecStart=(.+)$", text, re.M)
            args = shlex.split(commands[0]) if len(commands) == 1 else []
            environments = [item for line in re.findall(r"^Environment=(.+)$", text, re.M) for item in shlex.split(line)]
            matched = bool(args) and args[0].startswith(str(self.base / "runtime") + "/") and "__service-launcher" in args and "T3CODE_HOME=" + str(self.base) in environments
        if not matched:
            raise Blocked("The user service belongs to another or unknown T3 base_dir")
        if self.os == "linux":
            effective = run(["systemctl", "--user", "show", "--property=Environment", "--property=ExecStart", "t3code.service"]).stdout
            environment_lines = re.findall(r"^Environment=(.*)$", effective, re.M)
            environment = [item for line in environment_lines for item in shlex.split(line)]
            homes = [item.removeprefix("T3CODE_HOME=") for item in environment if item.startswith("T3CODE_HOME=")]
            executable = re.search(r"(?:^|[ {])path=(.*?)\s*;", effective)
            if homes != [str(self.base)] or not executable or not executable.group(1).startswith(str(self.base / "runtime") + "/"):
                raise Blocked("Effective service overrides target another or unknown T3 installation")
        result = run(["launchctl", "print", "gui/" + str(os.getuid()) + "/com.t3tools.t3code.service"] if self.os == "darwin" else ["systemctl", "--user", "is-active", "t3code.service"], check=False)
        running = result.returncode == 0 and (self.os != "darwin" or "state = running" in result.stdout)
        return True, running

    def busy(self, running):
        if not running:
            return "idle"
        recognized = False
        for path in self.databases():
            with contextlib.closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)) as db:
                tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
                for table in ("projection_turns", "orchestration_v2_projection_runs", "orchestration_v2_projection_provider_turns", "orchestration_v2_effect_outbox"):
                    if table not in tables:
                        continue
                    columns = {row[1] for row in db.execute('PRAGMA table_info("' + table + '")')}
                    field = next((c for c in ("state", "status") if c in columns), None)
                    recognized = recognized or bool(field)
                    if field and db.execute('SELECT 1 FROM "' + table + '" WHERE "' + field + '" IS NULL OR "' + field + '" NOT IN (?, ?, ?, ?, ?, ?, ?, ?) LIMIT 1', ("completed", "interrupted", "failed", "cancelled", "rolled_back", "succeeded", "stopped", "error")).fetchone():
                        return "busy"
        runtime_pid = read_json(self.runtime, {}).get("pid")
        if not recognized or not isinstance(runtime_pid, int):
            return "unknown"
        processes = self.processes()
        if runtime_pid not in processes:
            return "unknown"
        descendants = {runtime_pid}
        while True:
            children = {pid for pid, (parent, _) in processes.items() if parent in descendants}
            if children <= descendants:
                break
            descendants |= children
        service = read_json(self.base / "runtime/service-state.json", {})
        version = service.get("activeVersion", "")
        monitors = []
        if VERSION.fullmatch(version):
            directory = self.base / "runtime/versions" / version / "resource-monitor"
            if directory.is_dir():
                monitors = [str(path) for path in directory.rglob("t3-resource-monitor") if path.is_file() and not path.is_symlink()]
        for pid in descendants - {runtime_pid}:
            command = processes[pid][1]
            if not any(command == monitor or command.startswith(monitor + " ") for monitor in monitors):
                return "busy"
        return "idle"

    def processes(self):
        result = run(["ps", "-axo", "pid=,ppid=,command="])
        processes = {}
        for line in result.stdout.splitlines():
            fields = line.strip().split(None, 2)
            if len(fields) == 3 and fields[0].isdigit() and fields[1].isdigit():
                processes[int(fields[0])] = (int(fields[1]), fields[2])
        if not processes:
            raise Blocked("Cannot inspect process ancestry for safe update")
        return processes

    def require_external_coordinator(self, app):
        processes = self.processes()
        pid = os.getpid()
        seen = set()
        appimage_pids = set(self.appimage_processes(app)) if app and self.os == "linux" else set()
        while pid in processes and pid not in seen:
            seen.add(pid)
            parent, command = processes[pid]
            prefixes = [str(self.base / "runtime") + "/"]
            if app:
                prefixes.append(str(app) + "/")
            if pid in appimage_pids or any(command.startswith(prefix) or command.startswith('"' + prefix) for prefix in prefixes):
                raise Blocked("Run the updater from an external terminal; this process belongs to the selected T3 installation")
            pid = parent

    def databases(self):
        paths = []
        for directory in (self.base, self.base / "userdata"):
            if directory.is_dir():
                for path in directory.iterdir():
                    if path.suffix in (".sqlite", ".sqlite3", ".db") and path.is_file():
                        paths.append(owned(path))
        return sorted(set(paths))

    def health_url(self):
        state = read_json(self.runtime, {})
        value = self.request.get("health_url") or state.get("origin", "")
        if not value:
            return ""
        parsed = urllib.parse.urlparse(value)
        if parsed.scheme not in ("http", "https") or parsed.hostname not in ("localhost", "127.0.0.1", "::1") or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise Blocked("Host health URL must be a credential-free loopback HTTP(S) URL")
        return value

    def descriptor(self, origin):
        parsed = urllib.parse.urlparse(origin)
        url = urllib.parse.urlunparse((parsed.scheme, parsed.netloc, "/.well-known/t3/environment", "", "", ""))
        with open_url(url, NoRedirect, 3) as response:
            if response.status != 200:
                raise Blocked("T3 environment descriptor returned an error")
            return json.loads(response.read(64 * 1024))

    def probe(self):
        info = {"machine_id": self.machine_id(), "environment_id": self.environment_id(), "runtime_version": "", "os": self.os, "arch": self.arch, "version": "", "desktop_version": "", "base_dir": str(self.base), "cli_path": "", "app_path": "", "service_installed": False, "service_running": False, "service_version": "", "desktop_running": False, "busy": "unknown", "health_url": "", "supported": False, "blocker": ""}
        try:
            if self.request.get("expected_machine_id") and self.request["expected_machine_id"] != info["machine_id"]:
                raise Blocked("Device identity changed; enroll the correct device again")
            if self.os not in ("darwin", "linux") or self.arch == "unknown":
                raise Blocked("Unsupported operating system or CPU")
            cli, app = self.cli(), self.app()
            info["cli_path"] = str(cli) if cli else ""
            info["app_path"] = str(app) if app else ""
            if cli:
                owned(cli, links=True)
                resolved = owned(cli.resolve())
                if self.base / "runtime/versions" in cli.parents or (not cli.is_symlink() and self.base.resolve() / "runtime/versions" in resolved.parents):
                    raise Blocked("Select the external CLI launcher; direct native runtime paths must remain immutable")
                if any(part in ("node_modules", "Cellar", "Homebrew", "snap", "nix") for part in resolved.parts) or not resolved.is_file():
                    raise Blocked("Package-managed CLI needs its package manager's preview update route")
                if self.os == "linux":
                    for manager, arguments in (("dpkg-query", ["-S"]), ("rpm", ["-qf"])):
                        if shutil.which(manager) and run([manager, *arguments, resolved], check=False).returncode == 0:
                            raise Blocked("Package-managed CLI needs its package manager's preview update route")
                with resolved.open("rb") as stream:
                    if stream.read(2) == b"#!":
                        raise Blocked("Script/npm CLI is unsupported; enroll a standalone archive installation")
                info["version"] = parse_version(run([cli, "--version"]).stdout)
                if not VERSION.fullmatch(info["version"]):
                    raise Blocked("Selected CLI is not a recognized preview installation")
            if app:
                owned(app)
                if self.os == "darwin":
                    data = plistlib.loads((app / "Contents/Info.plist").read_bytes())
                    if data.get("CFBundleIdentifier") != "com.t3tools.t3code":
                        raise Blocked("Selected app has an unexpected bundle identity")
                    info["desktop_version"] = data.get("CFBundleShortVersionString", "")
                    if not VERSION.fullmatch(info["desktop_version"]):
                        raise Blocked("Selected desktop app is not a preview")
                else:
                    info["desktop_version"] = self.appimage_version(app)
                    if not VERSION.fullmatch(info["desktop_version"]):
                        raise Blocked("Selected AppImage is not a preview")
            installed, running = self.service_state()
            if installed or app:
                self.require_external_coordinator(app)
            info.update(service_installed=installed, service_running=running, health_url=self.health_url())
            service = read_json(self.base / "runtime/service-state.json", {})
            info["service_version"] = service.get("activeVersion", "") if running else ""
            info["desktop_running"] = self.desktop_running(app) if app else False
            if app and self.os == "linux" and (info["desktop_running"] or not installed) and not (os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY")):
                raise Blocked("A Linux desktop session is required to reopen and verify the selected AppImage")
            info["busy"] = self.busy(running or info["desktop_running"])
            if running or info["desktop_running"]:
                descriptor = self.descriptor(info["health_url"])
                environment = descriptor.get("environmentId", "")
                if info["environment_id"] and environment != info["environment_id"]:
                    raise Blocked("Runtime descriptor belongs to another T3 environment")
                info["environment_id"] = environment
                info["runtime_version"] = descriptor.get("serverVersion", "")
                info["service_version"] = info["runtime_version"] if running else ""
            if not cli:
                raise Blocked("A standalone preview CLI is required for staged migration and recovery")
            if not installed and not app and self.runtime.exists():
                raise Blocked("Unmanaged server runtime exists; a managed service is required for safe shutdown")
            if (installed or app) and not info["health_url"]:
                raise Blocked("No persisted runtime origin; enroll an explicit loopback health_url")
            if (installed or app) and not self.databases():
                raise Blocked("No active SQLite data found under the selected T3 base_dir")
            if not os.access(cli.parent, os.W_OK) or (app and not os.access(app.parent, os.W_OK)):
                raise Blocked("Installation parent directory is not writable by this user")
            info["supported"] = True
        except (Blocked, OSError, ValueError, sqlite3.Error) as error:
            info["blocker"] = str(error) if isinstance(error, Blocked) else "Cannot safely inspect installation metadata"
        return info

    def set_status(self, status, **values):
        self.journal.update(values)
        self.journal["status"] = status
        write_json(self.root / self.operation / "journal.json", self.journal)
        write_json(self.root / "active.json", {"operation": self.operation})

    def operation_dir(self):
        if not OPERATION.fullmatch(self.operation):
            raise Blocked("A safe unique operation identifier is required")
        directory = owned(self.root / self.operation)
        if directory.exists() and (directory / "owner").read_text() != MARKER:
            raise Blocked("Operation ownership marker is missing")
        return directory

    def signature(self, app):
        run(["codesign", "--verify", "--deep", "--strict=symlinks", app])
        details = run(["codesign", "-dv", "--verbose=4", app])
        team = re.search(r"^TeamIdentifier=([A-Z0-9]+)$", details.stderr, re.M)
        if not team:
            raise Blocked("App signature has no verified signing team")
        return team.group(1)

    def appimage_processes(self, app):
        processes = []
        for process in Path("/proc").iterdir():
            if not process.name.isdigit():
                continue
            try:
                if process.stat().st_uid != os.getuid():
                    continue
                environment = (process / "environ").read_bytes().split(b"\0")
                if b"APPIMAGE=" + os.fsencode(app) in environment:
                    args = (process / "cmdline").read_bytes().split(b"\0")
                    if not any(arg.startswith(b"--type=") for arg in args):
                        processes.append(int(process.name))
            except FileNotFoundError:
                continue
            except PermissionError:
                raise Blocked("Cannot inspect user processes to determine AppImage lifecycle") from None
        return processes

    def appimage_version(self, app):
        with app.open("rb") as stream:
            header = stream.read(20)
        if len(header) != 20 or header[:4] != b"\x7fELF" or header[8:11] != b"AI\x02":
            raise Blocked("Desktop executable is not a supported type-2 AppImage")
        cpu = int.from_bytes(header[18:20], "little" if header[5] == 1 else "big")
        if cpu != (183 if self.arch == "arm64" else 62):
            raise Blocked("AppImage CPU does not match this device")
        with tempfile.TemporaryDirectory(prefix="t3-preview-appimage-") as temp:
            isolated = Path(temp)
            env = dict(os.environ, HOME=temp, T3CODE_HOME=temp, T3_APP_HOME=temp)
            result = subprocess.run([str(app), "--appimage-extract", "resources/*"], cwd=isolated, env=env, capture_output=True, text=True, timeout=90)
            if result.returncode:
                raise Blocked("AppImage metadata extraction failed")
            resources = isolated / "squashfs-root/resources"
            package = resources / "app/package.json"
            if package.is_file():
                metadata = json.loads(package.read_text())
                if metadata.get("name") != "@t3tools/desktop":
                    raise Blocked("AppImage package identity is not T3 Code desktop")
                return metadata.get("version", "")
            with (resources / "app.asar").open("rb") as stream:
                prefix = stream.read(16)
                if len(prefix) != 16:
                    raise Blocked("AppImage contains an invalid app archive")
                _, header_size, _, json_size = struct.unpack("<4I", prefix)
                if json_size > 16 * 1024 * 1024 or header_size < json_size + 8:
                    raise Blocked("AppImage archive header is invalid")
                data = json.loads(stream.read(json_size))
                package = data["files"]["package.json"]
                if package.get("unpacked") or package.get("size", 0) > 1024 * 1024:
                    raise Blocked("AppImage package metadata is unsupported")
                stream.seek(8 + header_size + int(package["offset"]))
                metadata = json.loads(stream.read(package["size"]))
                if metadata.get("name") != "@t3tools/desktop":
                    raise Blocked("AppImage package identity is not T3 Code desktop")
                return metadata.get("version", "")

    def open_app(self, app):
        if self.os == "darwin":
            run(["open", "-a", app])
        else:
            if not (os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY")):
                raise Blocked("A Linux desktop session is required to reopen the AppImage")
            subprocess.Popen([str(app)], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)

    def desktop_running(self, app):
        if not app:
            return False
        if self.os == "darwin":
            script = 'on run argv\nset targetPath to item 1 of argv\ntell application "System Events"\nrepeat with p in application processes\ntry\nif POSIX path of (application file of p as alias) is targetPath & "/" then return "true"\nend try\nend repeat\nend tell\nreturn "false"\nend run'
            result = run(["osascript", "-e", script, str(app)])
            if result.stdout.strip() not in ("true", "false"):
                raise Blocked("Cannot determine the selected desktop app's running state")
            return result.stdout.strip() == "true"
        return bool(self.appimage_processes(app))

    def stop(self, info):
        if info.get("desktop_running"):
            if self.os == "darwin":
                run(["osascript", "-e", 'on run argv\ntell application (item 1 of argv) to quit\nend run', info["app_path"]])
            else:
                for pid in self.appimage_processes(Path(info["app_path"])):
                    os.kill(pid, signal.SIGTERM)
            for _ in range(30):
                if not self.desktop_running(Path(info["app_path"])):
                    break
                time.sleep(1)
            else:
                raise Blocked("Desktop did not quit gracefully; no process was force-killed")
        if info["service_running"]:
            if self.os == "darwin":
                run(["launchctl", "bootout", "--wait", "gui/" + str(os.getuid()) + "/com.t3tools.t3code.service"])
            else:
                run(["systemctl", "--user", "stop", "t3code.service"])
        if self.service_state()[1]:
            raise Blocked("Service is still running after graceful stop")
        old_runtime = read_json(self.runtime, {})
        pid = old_runtime.get("pid")
        if isinstance(pid, int) and pid > 1:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                pass
            else:
                raise Blocked("The recorded server process is still alive; backup boundary is not safe")

    def restart(self, info):
        if not info["service_installed"]:
            if info["app_path"]:
                self.open_app(Path(info["app_path"]))
            return
        if self.os == "darwin":
            run(["launchctl", "bootstrap", "gui/" + str(os.getuid()), self.unit])
        else:
            run(["systemctl", "--user", "daemon-reload"])
            run(["systemctl", "--user", "start", "t3code.service"])
        if info.get("desktop_running"):
            self.open_app(Path(info["app_path"]))

    def resume_original(self, info):
        if info["service_running"] and not self.service_state()[1]:
            self.restart(dict(info, desktop_running=False))
        if info["desktop_running"] and not self.desktop_running(Path(info["app_path"])):
            self.open_app(Path(info["app_path"]))

    def reset_active(self):
        active = read_json(self.root / "active.json", {})
        if active.get("operation") == self.operation:
            write_json(self.root / "active.json", read_json(self.root / "latest.json", {}))

    def prepare(self):
        if not VERSION.fullmatch(self.target):
            raise Blocked("Target must be an exact preview version without the v prefix")
        info = self.probe()
        if not info["supported"]:
            raise Blocked(info["blocker"])
        targets = [Path(info["cli_path"]).parent]
        if info["service_installed"]:
            targets.append(self.unit.parent)
        if any(path.stat().st_dev != self.base.stat().st_dev for path in targets):
            raise Blocked("CLI, service configuration, and backup directory must share one filesystem for safe rollback")
        active = read_json(self.root / "active.json", {}).get("operation")
        if active:
            if not OPERATION.fullmatch(active):
                raise Blocked("Invalid active operation identifier")
            previous = read_json(self.root / active / "journal.json", {})
            if previous.get("status") in ("stopping", "backed-up", "activating", "verifying", "recovery-required", "rolling-back"):
                raise Blocked("An interrupted operation requires recovery before another update")
        directory = self.operation_dir()
        if directory.exists():
            previous = read_json(directory / "journal.json", {})
            if previous.get("target") == self.target and previous.get("status") == "staged":
                return dict(info, status="staged", operation=self.operation)
            raise Blocked("Operation already exists; inspect status or use a new operation identifier")
        assets = self.request.get("assets", [])
        cli_assets = [a for a in assets if a.get("name", "").endswith(".tar.gz") and self.os in a.get("name", "") and self.arch in a.get("name", "")]
        if len(cli_assets) != 1:
            raise Blocked("Exactly one matching standalone CLI archive is required")
        app_assets = [a for a in assets if a.get("name", "").endswith(".dmg" if self.os == "darwin" else ".AppImage") and self.arch in a.get("name", "")]
        if info["app_path"] and len(app_assets) != 1:
            raise Blocked("Exactly one matching desktop asset with a release digest is required")
        required = tree_size(self.base / "userdata") * 2 + tree_size(self.base / "runtime") + tree_size(Path(info["cli_path"]).resolve()) + sum(a.get("size", 0) for a in assets) * 6
        if info["app_path"]:
            required += tree_size(Path(info["app_path"])) * 3
            if self.os == "darwin":
                self.signature(Path(info["app_path"]))
            if Path(info["app_path"]).parent.stat().st_dev != self.base.stat().st_dev:
                raise Blocked("App and updater backup directory must be on the same filesystem")
        if shutil.disk_usage(self.base).free < required + 256 * 1024 * 1024:
            raise Blocked("Insufficient free space for staging and overlapping rollback backups")
        directory.mkdir(mode=0o700)
        (directory / "owner").write_text(MARKER)
        self.journal = {"operation": self.operation, "target": self.target, "status": "planned", "machine_id": self.machine_id(), "info": info, "required_bytes": required}
        self.set_status("planned")
        stage = directory / "stage"
        stage.mkdir(mode=0o700)
        try:
            archive = stage / "cli.tar.gz"
            download(cli_assets[0], archive, self.target)
            unpacked = stage / "cli"
            unpacked.mkdir()
            extract_archive(archive, unpacked, shutil.disk_usage(stage).free - 256 * 1024 * 1024)
            candidates = [p for p in unpacked.rglob("t3") if p.is_file() and os.access(p, os.X_OK)]
            if len(candidates) != 1:
                raise Blocked("CLI archive does not contain exactly one executable named t3")
            cli = candidates[0]
            self.verify_runtime_artifacts(cli, required=False)
            isolated = stage / "preflight"
            isolated.mkdir()
            env = dict(os.environ, HOME=str(isolated), T3CODE_HOME=str(isolated), T3_APP_HOME=str(isolated), XDG_CONFIG_HOME=str(isolated))
            result = subprocess.run([str(cli), "--version"], env=env, cwd=isolated, capture_output=True, text=True, timeout=30)
            if result.returncode or parse_version(result.stdout) != self.target:
                raise Blocked("Staged executable version does not match the exact target")
            staged_app = None
            if info["app_path"] and self.os == "linux":
                staged_app = stage / "desktop.AppImage"
                download(app_assets[0], staged_app, self.target)
                staged_app.chmod(Path(info["app_path"]).stat().st_mode & 0o777)
                if self.appimage_version(staged_app) != self.target:
                    raise Blocked("Staged AppImage version does not match the target")
            if info["app_path"] and self.os == "darwin":
                dmg = stage / "desktop.dmg"
                download(app_assets[0], dmg, self.target)
                run(["hdiutil", "verify", dmg])
                mounted = plistlib.loads(run(["hdiutil", "attach", "-readonly", "-nobrowse", "-plist", dmg]).stdout.encode())
                mounts = [Path(e["mount-point"]) for e in mounted.get("system-entities", []) if "mount-point" in e]
                self.set_status("planned", mounts=[str(mount) for mount in mounts])
                try:
                    if len(mounts) != 1:
                        raise Blocked("DMG must contain exactly one mountable volume")
                    apps = list(mounts[0].glob("*.app"))
                    if len(apps) != 1:
                        raise Blocked("DMG must contain exactly one app bundle")
                    data = plistlib.loads((apps[0] / "Contents/Info.plist").read_bytes())
                    if data.get("CFBundleIdentifier") != "com.t3tools.t3code" or data.get("CFBundleShortVersionString") != self.target:
                        raise Blocked("Desktop bundle identity or version mismatch")
                    if self.signature(apps[0]) != self.signature(Path(info["app_path"])):
                        raise Blocked("Desktop signing team changed")
                    run(["spctl", "--assess", "--type", "execute", apps[0]])
                    staged_app = stage / "desktop.app"
                    run(["ditto", "--rsrc", "--extattr", apps[0], staged_app])
                    original = Path(info["app_path"])
                    icon = original / "Icon\r"
                    if icon.exists():
                        run(["ditto", "--rsrc", "--extattr", icon, staged_app / "Icon\r"])
                    finder = run(["xattr", "-px", "com.apple.FinderInfo", original], check=False)
                    if finder.returncode == 0:
                        value = re.sub(r"\s+", "", finder.stdout)
                        if not re.fullmatch(r"[a-fA-F0-9]{64}", value):
                            raise Blocked("Existing Finder metadata has an unexpected format")
                        run(["xattr", "-wx", "com.apple.FinderInfo", value, staged_app])
                    self.signature(staged_app)
                finally:
                    self.detach_mounts()
            hashes = {str(p.relative_to(stage)): digest(p) for p in stage.rglob("*") if p.is_file() and not p.is_symlink()}
            self.set_status("staged", cli=str(cli.relative_to(directory)), app=str(staged_app.relative_to(directory)) if staged_app else "", hashes=hashes)
            return dict(info, status="staged", operation=self.operation, required_bytes=required)
        except Exception:
            self.set_status("prepare-failed")
            if stage.exists() and not self.journal.get("mounts"):
                shutil.rmtree(stage)
            raise

    def detach_mounts(self):
        remaining = []
        for mount in self.journal.get("mounts", []):
            try:
                run(["hdiutil", "detach", mount])
            except Exception:
                remaining.append(mount)
        self.set_status(self.journal["status"], mounts=remaining)
        if remaining:
            raise Blocked("A tool-mounted installer could not be detached; its journal and DMG were retained for cleanup")

    def backup(self, directory, info):
        backup = directory / "backup"
        backup.mkdir(mode=0o700)
        paths = [self.base / "userdata", self.base / "runtime", self.unit, Path(info["cli_path"])]
        if self.os == "linux":
            paths.append(Path(str(self.unit) + ".d"))
        for path in self.base.iterdir():
            if path.is_file() and not path.name.endswith(("-wal", "-shm")) and path.name != "update-preview":
                paths.append(path)
        records, summaries = [], {}
        databases = set(self.databases())
        for index, source in enumerate(dict.fromkeys(paths)):
            if not source.exists() and not source.is_symlink():
                continue
            owned(source, links=source == Path(info["cli_path"]))
            target = backup / str(index)
            if source == self.base / "userdata":
                target.mkdir()
                for child in source.iterdir():
                    if child.name in ("logs", "server-runtime.json") or child.name.endswith(("-wal", "-shm")):
                        continue
                    if child in databases:
                        summaries[str(child)] = snapshot_db(child, target / child.name)
                    else:
                        copy_path(child, target / child.name)
            elif source == self.base / "runtime":
                target.mkdir()
                for child in source.iterdir():
                    if child.name != "versions":
                        copy_path(child, target / child.name)
                state = read_json(source / "service-state.json", {})
                versions = {state.get("activeVersion"), info["version"]}
                for version in versions:
                    if version and VERSION.fullmatch(version) and (source / "versions" / version).is_dir():
                        (target / "versions").mkdir(exist_ok=True)
                        copy_path(source / "versions" / version, target / "versions" / version)
            elif source in databases:
                summaries[str(source)] = snapshot_db(source, target)
            else:
                copy_path(source, target)
            records.append({"source": str(source), "backup": str(target.relative_to(directory))})
        cli_runtime = backup / "cli-runtime"
        cli_runtime.mkdir()
        executable = Path(info["cli_path"]).resolve()
        copy_path(executable, cli_runtime / "t3")
        for name in ("client", "node_modules", "resource-monitor", ".install-complete"):
            sibling = executable.parent / name
            if sibling.exists():
                copy_path(sibling, cli_runtime / name)
        if digest(executable) != digest(cli_runtime / "t3"):
            raise Blocked("CLI recovery binary verification failed")
        if info["app_path"]:
            app_target = backup / "desktop.app"
            if self.os == "darwin":
                run(["ditto", "--rsrc", "--extattr", info["app_path"], app_target])
                self.signature(app_target)
            else:
                copy_path(Path(info["app_path"]), app_target)
            records.append({"source": info["app_path"], "backup": str(app_target.relative_to(directory))})
        inventory = {r["source"]: data_inventory(Path(r["source"])) for r in records if Path(r["source"]) == self.base / "userdata" or (Path(r["source"]).parent == self.base and Path(r["source"]).name != "runtime")}
        self.set_status("backed-up", backups=records, databases=summaries, before_inventory=inventory, service_guard=self.service_inventory() if info["service_installed"] else None, backup=str(backup), backup_manifest=file_manifest(backup), cli_backup=str(cli_runtime.relative_to(directory)), cli_original_target=str(executable))

    def service_inventory(self):
        if self.os == "darwin":
            metadata = plistlib.loads(self.unit.read_bytes())
            arguments = metadata.pop("ProgramArguments", [])
            value = plistlib.dumps(metadata, sort_keys=True)
        else:
            text = self.unit.read_text()
            commands = re.findall(r"^ExecStart=(.+)$", text, re.M)
            arguments = shlex.split(commands[0]) if len(commands) == 1 else []
            value = re.sub(r"^ExecStart=.+$", "", text, flags=re.M).encode()
        if not arguments:
            raise Blocked("Service command cannot be identified for recovery")
        return {"metadata": hashlib.sha256(value).hexdigest(), "arguments": arguments[1:], "launcher": arguments[0], "dropins": data_inventory(Path(str(self.unit) + ".d")) if self.os == "linux" else {}}

    def verify_service_inventory(self):
        expected = self.journal.get("service_guard")
        if expected is None:
            return
        current = self.service_inventory()
        allowed = {expected["launcher"]}
        if self.journal.get("activation_started"):
            allowed.add(str(self.base / "runtime/versions" / self.journal["target"] / "t3"))
        if current["launcher"] not in allowed or any(current[key] != expected[key] for key in ("metadata", "arguments", "dropins")):
            raise Blocked("Service settings changed after backup; rollback would discard newer configuration")

    def verify(self, info, version, server_version=None):
        server_version = server_version or version
        if parse_version(run([info["cli_path"], "--version"]).stdout) != version:
            raise Blocked("Installed CLI version does not match the target")
        if not info["service_installed"] and not info["app_path"]:
            return {name: db_summary(Path(name)) for name in self.journal["databases"]}
        stable = None
        for _ in range(30):
            state = read_json(self.runtime, {})
            service = read_json(self.base / "runtime/service-state.json", {})
            pid = state.get("pid")
            service_ok = not info["service_installed"] or (service.get("activeVersion") == server_version and self.service_state()[1])
            if isinstance(pid, int) and pid > 1 and service_ok:
                command = run(["ps", "-p", str(pid), "-o", "command="], check=False).stdout.strip()
                expected = str(self.base / "runtime/versions" / server_version / "t3") if info["service_installed"] else info["app_path"] + "/"
                process_matches = command == expected or command.startswith(expected + " ") or (not info["service_installed"] and expected in command)
                if not info["service_installed"] and self.os == "linux":
                    process_matches = pid in self.appimage_processes(Path(info["app_path"]))
                if process_matches:
                    try:
                        descriptor = self.descriptor(info["health_url"])
                        healthy = descriptor.get("serverVersion") == server_version and bool(descriptor.get("environmentId"))
                        if info["environment_id"]:
                            healthy = healthy and descriptor.get("environmentId") == info["environment_id"]
                        if healthy and stable == pid:
                            self.journal["verified_environment_id"] = descriptor["environmentId"]
                            break
                        stable = pid if healthy else None
                    except OSError:
                        stable = None
                else:
                    stable = None
            else:
                stable = None
            time.sleep(2)
        else:
            raise Blocked("Runtime version, stable process, and HTTP health were not all verified")
        current = {}
        for name, before in self.journal["databases"].items():
            after = db_summary(Path(name))
            record = next(r for r in self.journal["backups"] if Path(r["source"]) in (Path(name), Path(name).parent))
            saved = self.root / self.operation / record["backup"]
            if Path(record["source"]) == Path(name).parent:
                saved = saved / Path(name).name
            verify_preserved_rows(Path(name), saved)
            current[name] = after
        return current

    def verify_runtime_artifacts(self, staged_cli, required=True, version=None):
        version = version or self.target
        if not VERSION.fullmatch(version):
            raise Blocked("Invalid native runtime version")
        installed = owned(self.base / "runtime/versions" / version)
        if not installed.exists() and not required:
            return
        if not installed.is_dir():
            raise Blocked("Target native runtime directory was not installed")
        expected = file_manifest(staged_cli.parent)
        actual = file_manifest(installed)
        sentinel = actual.pop(".install-complete", None)
        expected.pop(".install-complete", None)
        sentinel_path = owned(installed / ".install-complete")
        if actual != expected or (sentinel is not None and sentinel_path.read_text().strip() != version):
            raise Blocked("Installed target runtime does not match the verified official archive")

    def install_native_runtime(self, staged_cli, version=None):
        version = version or self.target
        self.verify_runtime_artifacts(staged_cli, required=False, version=version)
        installed = owned(self.base / "runtime/versions" / version)
        if not installed.exists():
            installed.parent.mkdir(parents=True, exist_ok=True)
            stage = owned(self.operation_dir() / "stage")
            stage.mkdir(exist_ok=True)
            temporary = stage / ("native-" + version)
            copy_path(staged_cli.parent, temporary)
            (temporary / ".install-complete").write_text(version)
            os.rename(temporary, installed)
        elif not (installed / ".install-complete").exists():
            (installed / ".install-complete").write_text(version)
        self.verify_runtime_artifacts(staged_cli, version=version)
        return installed / "t3"

    def apply(self):
        directory = self.operation_dir()
        self.journal = read_json(directory / "journal.json", {})
        if self.journal.get("target") != self.target or self.journal.get("machine_id") != self.machine_id():
            raise Blocked("No matching verified staged operation for this device and target")
        if self.journal.get("status") == "healthy":
            self.verify(self.journal["info"], self.journal["target"])
            return dict(self.journal["info"], version=self.journal["target"], runtime_version=self.journal["target"] if self.journal["info"]["service_installed"] or self.journal["info"]["app_path"] else "", service_version=self.journal["target"] if self.journal["info"]["service_installed"] else "", desktop_version=self.journal["target"] if self.journal["info"]["app_path"] else "", status="healthy", backup=self.journal["backup"])
        if self.journal.get("status") != "staged":
            raise Blocked("No matching verified staged operation")
        info = self.probe()
        if not info["supported"]:
            raise Blocked(info["blocker"])
        targets = [Path(info["cli_path"]).parent]
        if info["service_installed"]:
            targets.append(self.unit.parent)
        if info["app_path"]:
            targets.append(Path(info["app_path"]).parent)
        if any(path.stat().st_dev != self.base.stat().st_dev for path in targets):
            raise Blocked("Installation paths changed filesystems after preparation")
        if info != self.journal["info"]:
            changed = [key for key in ("cli_path", "app_path", "version", "desktop_version", "machine_id") if info[key] != self.journal["info"][key]]
            if changed:
                raise Blocked("Installation changed after staging: " + ", ".join(changed))
        if not self.request.get("force") and info["busy"] != "idle":
            return dict(info, status="deferred", deferred=True, error="Device is busy or terminal work cannot be proven idle; retry later or explicitly use --force")
        info["desktop_running"] = self.desktop_running(Path(info["app_path"])) if info["app_path"] else False
        stage = directory / "stage"
        for name, expected in self.journal["hashes"].items():
            path = owned(stage / name)
            if not path.is_file() or digest(path) != expected:
                raise Blocked("Staged artifact changed after preparation")
        if shutil.disk_usage(self.base).free < self.journal["required_bytes"]:
            raise Blocked("Free disk space fell below the backup requirement")
        service_before = read_json(self.base / "runtime/service-state.json", {})
        self.set_status("stopping", info=info, restore_service_version=service_before.get("activeVersion", info["version"]))
        try:
            self.stop(info)
            self.backup(directory, info)
            self.set_status("activating", activation_started=True)
            staged_cli = directory / self.journal["cli"]
            active_cli = self.install_native_runtime(staged_cli)
            if info["service_installed"]:
                environment = dict(os.environ)
                environment.pop("T3CODE_RELEASE_BASE_URL", None)
                result = subprocess.run([str(active_cli), "update", self.target, "--base-dir", str(self.base)], stdin=subprocess.DEVNULL, env=environment, capture_output=True, text=True, timeout=300)
                if result.returncode:
                    raise Blocked("Target CLI could not install the service without restarting it")
                if self.service_state()[1]:
                    raise Blocked("Target CLI unexpectedly started the service before configuration restore")
                self.verify_runtime_artifacts(staged_cli)
                original_unit = next(directory / r["backup"] for r in self.journal["backups"] if r["source"] == str(self.unit))
                self.preserve_unit(original_unit)
            cli_path = Path(info["cli_path"])
            temporary = cli_path.parent / (".t3-update-preview-" + self.operation)
            if temporary.exists() or temporary.is_symlink():
                raise Blocked("Temporary CLI launcher path already exists")
            temporary.symlink_to(active_cli)
            os.replace(temporary, cli_path)
            if self.journal["app"]:
                app = Path(info["app_path"])
                retired = directory / "retired.app"
                os.rename(app, retired)
                try:
                    os.rename(directory / self.journal["app"], app)
                except Exception:
                    os.rename(retired, app)
                    raise
            self.set_status("verifying")
            if info["app_path"] and not info["service_installed"]:
                self.open_app(Path(info["app_path"]))
            current = self.verify(info, self.target)
            if info["desktop_running"] and info["service_installed"]:
                self.open_app(Path(info["app_path"]))
                if not self.desktop_running(Path(info["app_path"])):
                    raise Blocked("Updated desktop app did not reopen")
            previous = read_json(self.root / "latest.json", {})
            inventory = {name: data_inventory(Path(name)) for name in self.journal["before_inventory"]}
            self.set_status("healthy", after_databases=current, after_inventory=inventory, version=self.target, obsolete_backup=previous.get("operation", ""))
            write_json(self.root / "latest.json", {"operation": self.operation})
            self.clean_stage(directory)
            old = previous.get("operation")
            if old and old != self.operation:
                self.remove_owned(self.root / old)
            return dict(info, version=self.target, environment_id=self.journal.get("verified_environment_id", info["environment_id"]), runtime_version=self.target if info["service_installed"] or info["app_path"] else "", service_version=self.target if info["service_installed"] else "", service_running=info["service_installed"], desktop_version=self.target if info["app_path"] else "", status="healthy", backup=self.journal["backup"])
        except Exception:
            if self.journal.get("status") == "healthy":
                self.set_status("healthy", cleanup_pending=True)
                raise
            if not self.journal.get("activation_started"):
                try:
                    self.resume_original(info)
                    self.set_status("aborted-safe")
                    self.clean_stage(directory)
                    self.reset_active()
                except Exception:
                    self.set_status("recovery-required")
                raise
            self.set_status("recovery-required")
            self.clean_stage(directory)
            raise

    def preserve_unit(self, original):
        if self.os == "darwin":
            old = plistlib.loads(original.read_bytes())
            new = plistlib.loads(self.unit.read_bytes())
            old["ProgramArguments"] = new["ProgramArguments"]
            self.unit.write_bytes(plistlib.dumps(old))
            if self.service_state()[1]:
                run(["launchctl", "bootout", "--wait", "gui/" + str(os.getuid()) + "/com.t3tools.t3code.service"])
            run(["launchctl", "bootstrap", "gui/" + str(os.getuid()), self.unit])
        else:
            old = original.read_text()
            new = self.unit.read_text()
            command = re.findall(r"^ExecStart=(.+)$", new, re.M)
            if len(command) != 1 or len(re.findall(r"^ExecStart=", old, re.M)) != 1:
                raise Blocked("Cannot preserve a service with multiple ExecStart entries")
            self.unit.write_text(re.sub(r"^ExecStart=.+$", lambda _: "ExecStart=" + command[0], old, flags=re.M))
            run(["systemctl", "--user", "daemon-reload"])
            run(["systemctl", "--user", "restart", "t3code.service"])

    def clean_stage(self, directory):
        for name in ("stage", "retired.app"):
            path = owned(directory / name)
            if path.is_dir():
                shutil.rmtree(path)
            elif path.exists():
                path.unlink()

    def original_launcher_target(self, directory, info, restore=False):
        record = next(r for r in self.journal["backups"] if r["source"] == info["cli_path"])
        saved_launcher = directory / record["backup"]
        if not saved_launcher.is_symlink():
            return
        target = owned(Path(self.journal["cli_original_target"]))
        link = Path(os.readlink(saved_launcher))
        resolved_link = (link if link.is_absolute() else Path(info["cli_path"]).parent / link).resolve()
        if resolved_link != target:
            raise Blocked("Original CLI launcher link now resolves to a different installation")
        saved_runtime = directory / self.journal["cli_backup"]
        if not target.exists():
            native = self.base / "runtime/versions" / info["version"] / "t3"
            if target != native or target.parent.exists():
                raise Blocked("Original CLI link target is missing outside a recoverable native runtime")
            if restore:
                self.install_native_runtime(saved_runtime / "t3", version=info["version"])
            return
        for relative, expected in file_manifest(saved_runtime).items():
            candidate = target if relative == "t3" else target.parent / relative
            if not candidate.exists() and not candidate.is_symlink():
                raise Blocked("Original CLI runtime contents changed after backup")
            actual = "link:" + os.readlink(candidate) if candidate.is_symlink() else digest(candidate)
            if actual != expected:
                raise Blocked("Original CLI runtime contents changed after backup")

    def rollback(self):
        if not self.operation:
            self.operation = read_json(self.root / "active.json", read_json(self.root / "latest.json", {})).get("operation", "")
            if self.operation and OPERATION.fullmatch(self.operation):
                active = read_json(self.root / self.operation / "journal.json", {})
                if not active.get("backups") and active.get("status") in ("planned", "staged", "prepare-failed", "aborted-safe"):
                    self.operation = read_json(self.root / "latest.json", {}).get("operation", "")
        directory = self.operation_dir()
        self.journal = read_json(directory / "journal.json", {})
        if self.journal.get("machine_id") != self.machine_id():
            raise Blocked("Recovery backup belongs to another device")
        if self.journal.get("status") in ("stopping", "recovery-required") and not self.journal.get("activation_started") and self.journal.get("info"):
            self.resume_original(self.journal["info"])
            self.set_status("aborted-safe")
            self.reset_active()
            return dict(self.journal["info"], status="healthy", recovery="original-installation-resumed")
        if self.journal.get("status") not in ("healthy", "recovery-required", "backed-up", "activating", "verifying") or not self.journal.get("backups"):
            raise Blocked("No complete recovery backup for this operation")
        info = self.journal["info"]
        if file_manifest(Path(self.journal["backup"])) != self.journal.get("backup_manifest"):
            raise Blocked("Recovery backup contents changed; restoration is refused")
        self.original_launcher_target(directory, info)
        self.verify_service_inventory()
        inventory = self.journal.get("before_inventory")
        if inventory is None:
            raise Blocked("Recovery backup has no data inventory guard")
        for name, expected in inventory.items():
            if data_inventory(Path(name)) != expected:
                raise Blocked("Data or settings changed after the recovery checkpoint; rollback would discard newer work")
        guards = self.journal["databases"]
        for name, expected in guards.items():
            if db_summary(Path(name))["fingerprint"] != expected["fingerprint"]:
                raise Blocked("Database changed after the recovery checkpoint; rollback would discard newer work")
        current = dict(info)
        current["service_installed"], current["service_running"] = self.service_state()
        current["desktop_running"] = self.desktop_running(Path(info["app_path"])) if info["app_path"] else False
        self.require_external_coordinator(Path(info["app_path"]) if info["app_path"] else None)
        current["busy"] = self.busy(current["service_running"] or current["desktop_running"])
        if not self.request.get("force") and current["busy"] != "idle":
            return dict(current, status="deferred", deferred=True, error="Device is busy or idle state is unknown; use --force only to authorize interruption")
        self.stop(current)
        self.verify_service_inventory()
        for name, expected in inventory.items():
            if data_inventory(Path(name)) != expected:
                raise Blocked("Data or settings changed while stopping; automatic rollback is refused")
        for name, expected in guards.items():
            if db_summary(Path(name))["fingerprint"] != expected["fingerprint"]:
                raise Blocked("Database changed while stopping; automatic rollback is refused")
        self.set_status("rolling-back")
        try:
            for index, record in enumerate(self.journal["backups"]):
                source = owned(Path(record["source"]), links=True)
                backup = owned(directory / record["backup"], links=source == Path(info["cli_path"]))
                if source == self.base / "runtime":
                    source.mkdir(exist_ok=True)
                    for child in backup.iterdir():
                        if child.name == "versions":
                            (source / "versions").mkdir(exist_ok=True)
                            for version in child.iterdir():
                                self.install_native_runtime(version / "t3", version=version.name)
                            continue
                        target = owned(source / child.name)
                        retired = directory / ("rollback-runtime-" + child.name)
                        if target.exists():
                            os.rename(target, retired)
                        copy_path(child, target)
                    continue
                retired = directory / ("rollback-retired-" + str(index))
                if retired.exists():
                    raise Blocked("Prior rollback evidence exists; manual recovery is required")
                if source.exists() or source.is_symlink():
                    os.rename(source, retired)
                if source.suffix == ".app" and self.os == "darwin":
                    run(["ditto", "--rsrc", "--extattr", backup, source])
                else:
                    copy_path(backup, source)
            self.original_launcher_target(directory, info, restore=True)
            self.restart(info)
            restored_server_version = self.journal["restore_service_version"] if info["service_installed"] else info["desktop_version"]
            self.verify(info, info["version"], restored_server_version)
            self.set_status("rolled-back")
            self.clean_stage(directory)
            return dict(info, runtime_version=restored_server_version if info["service_installed"] or info["app_path"] else "", service_version=restored_server_version if info["service_installed"] else "", status="healthy", recovery="rolled-back", backup=self.journal["backup"])
        except Exception:
            self.set_status("recovery-required")
            raise

    def execute(self):
        action = self.request.get("action", "probe")
        if action == "probe":
            return self.probe()
        if action == "status":
            result = self.probe()
            if self.root.exists():
                self.state_dir()
                operation = self.operation or read_json(self.root / "active.json", read_json(self.root / "latest.json", {})).get("operation", "")
                if operation:
                    if not OPERATION.fullmatch(operation):
                        raise Blocked("Invalid operation identifier")
                    journal = read_json(self.root / operation / "journal.json", {})
                    result.update({key: journal[key] for key in ("status", "operation", "backup", "target") if key in journal})
            return result
        if action not in ("prepare", "apply", "rollback", "cleanup"):
            raise Blocked("Unknown worker action")
        with self.lock():
            if self.request.get("expected_machine_id") and self.request["expected_machine_id"] != self.machine_id():
                raise Blocked("Device identity changed")
            if action == "prepare":
                return self.prepare()
            if action == "apply":
                return self.apply()
            if action == "rollback":
                return self.rollback()
            if self.operation:
                directory = self.operation_dir()
                if not directory.exists():
                    return {"status": "cleaned"}
                journal = read_json(directory / "journal.json", {})
                if journal.get("mounts"):
                    self.journal = journal
                    self.detach_mounts()
                if journal.get("status") in ("stopping", "backed-up", "activating", "verifying", "recovery-required", "rolling-back"):
                    raise Blocked("Interrupted operation requires recovery; its files were retained")
                if journal.get("status") in ("planned", "staged", "prepare-failed", "aborted-safe"):
                    self.remove_owned(directory)
                    self.reset_active()
                else:
                    self.clean_stage(directory)
                    old = journal.get("obsolete_backup")
                    if old and old != self.operation and (self.root / old).exists():
                        self.remove_owned(self.root / old)
                    journal["cleanup_pending"] = False
                    write_json(directory / "journal.json", journal)
            return {"status": "cleaned"}


def main():
    os.umask(0o077)
    worker = None
    try:
        raw = sys.stdin.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise Blocked("Request exceeds size limit")
        worker = Worker(json.loads(raw))
        result = worker.execute()
        print(json.dumps(result))
        return 3 if result.get("deferred") else 0
    except Exception as error:
        result = {"error": str(error) if isinstance(error, Blocked) else "Host operation failed (" + type(error).__name__ + "); inspect the private recovery journal", "status": "blocked"}
        if worker and worker.journal:
            result["status"] = worker.journal.get("status", "blocked")
            if worker.journal.get("backup"):
                result["backup"] = worker.journal["backup"]
        print(json.dumps(result))
        return 1


if __name__ == "__main__":
    sys.exit(main())
