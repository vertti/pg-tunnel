# /// script
# requires-python = ">=3.11"
# dependencies = ["jupyterlab==4.6.3", "ipykernel==7.3.0", "jupyter-client==8.10.0", "psycopg2-binary==2.9.13"]
# ///
"""Opt-in live check: interrupt only this test's SSM connection; never replay user SQL."""

import argparse
import importlib.metadata
import json
import os
import platform
import secrets
import select
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

from jupyter_client import BlockingKernelClient


class Proxy(socketserver.ThreadingTCPServer):
    """Opaque CONNECT relay: AWS TLS remains end-to-end; no payload logging."""

    daemon_threads = True

    def __init__(self):
        self.lock = threading.Lock()
        self.streams = set()
        self.connections = 0
        self.rejected = 0
        self.block_until = 0
        super().__init__(("127.0.0.1", 0), Relay)
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def interrupt(self, seconds):
        with self.lock:
            if len(self.streams) != 1:
                raise RuntimeError("expected exactly one SSM data-channel connection")
            self.block_until = time.monotonic() + seconds
            for sock in self.streams:
                sock.shutdown(socket.SHUT_RDWR)
            return self.connections


class Relay(socketserver.StreamRequestHandler):
    def handle(self):
        self.request.settimeout(15)
        request = self.rfile.readline(8192).decode("ascii").split()
        if len(request) != 3 or request[0] != "CONNECT":
            return
        while self.rfile.readline(8192) not in (b"\r\n", b"\n", b""):
            pass
        host, port = request[1].rsplit(":", 1)
        if port != "443" or not host.endswith((".amazonaws.com", ".amazonaws.com.cn")):
            return
        is_stream = host.startswith("ssmmessages.")
        with self.server.lock:
            if is_stream and time.monotonic() < self.server.block_until:
                self.server.rejected += 1
                self.wfile.write(
                    b"HTTP/1.1 503 Test outage\r\nContent-Length: 0\r\n\r\n"
                )
                return
        try:
            with socket.create_connection((host, 443), timeout=10) as remote:
                self.request.settimeout(None)
                remote.settimeout(None)
                self.wfile.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
                if is_stream:
                    with self.server.lock:
                        self.server.streams.add(self.request)
                        self.server.connections += 1
                while True:
                    readable, _, _ = select.select([self.request, remote], [], [], 1)
                    for sock in readable:
                        data = sock.recv(65536)
                        if not data:
                            return
                        (remote if sock is self.request else self.request).sendall(data)
        except OSError:
            pass  # Closing the test connection is the injected failure.
        finally:
            with self.server.lock:
                self.server.streams.discard(self.request)


def check(args, root):
    (root / "notebooks").mkdir()
    (root / "runtime").mkdir(mode=0o700)
    spec = root / "data/kernels/recovery-check"
    spec.mkdir(parents=True)
    (spec / "kernel.json").write_text(
        json.dumps(
            {
                "argv": [
                    sys.executable,
                    "-m",
                    "ipykernel_launcher",
                    "-f",
                    "{connection_file}",
                ],
                "display_name": "Temporary recovery check",
                "language": "python",
            }
        )
    )
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    token = secrets.token_hex(32)
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def api(method, path, body=None):
        request = urllib.request.Request(
            f"http://127.0.0.1:{port}{path}",
            method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers={
                "Authorization": "token " + token,
                "Content-Type": "application/json",
            },
        )
        with http.open(request, timeout=5) as response:
            data = response.read()
            return json.loads(data) if data else None

    proxy = Proxy()
    env = dict(
        os.environ,
        JUPYTER_TOKEN=token,
        JUPYTER_CONFIG_DIR=str(root / "config"),
        JUPYTER_RUNTIME_DIR=str(root / "runtime"),
        JUPYTER_PATH=str(root / "data"),
        IPYTHONDIR=str(root / "ipython"),
        NO_PROXY="127.0.0.1,localhost,169.254.169.254,169.254.170.2",
        HTTPS_PROXY=f"http://127.0.0.1:{proxy.server_address[1]}",
    )
    command = [
        str(args.executable),
        "run",
        "--config",
        str(args.config),
        args.connection,
        "--",
        sys.executable,
        "-m",
        "jupyterlab",
        "--no-browser",
        "--ServerApp.ip=127.0.0.1",
        f"--ServerApp.port={port}",
        "--ServerApp.port_retries=0",
        f"--ServerApp.root_dir={root / 'notebooks'}",
    ]
    record = root / "connection.json"
    cell = """
import configparser, json, os
from pathlib import Path
from contextlib import closing
import psycopg2
settings = {key: os.environ[key] for key in ('PGSERVICE', 'PGSERVICEFILE', 'PGPASSFILE')}
service = configparser.ConfigParser(interpolation=None)
service.read(settings['PGSERVICEFILE'])
service = service['pg-tunnel']
with closing(psycopg2.connect('', connect_timeout=5)) as conn:
    conn.set_session(readonly=True)
    with conn.cursor() as cursor:
        cursor.execute("SELECT pg_backend_pid(), current_user, current_database(), current_setting('transaction_read_only'), ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()")
        backend, user, database, readonly, ssl = cursor.fetchone()
        assert (user, database, readonly, ssl) == (service['user'], service['dbname'], 'on', True)
Path(RECORD).write_text(json.dumps({'pid': os.getpid(), 'backend': backend, 'settings': settings, 'port': int(service['port'])}))
""".replace("RECORD", repr(str(record)))
    client = None
    proc = None
    suspended = []
    log = (root / "server.log").open("w")
    os.chmod(root / "server.log", 0o600)
    try:
        proc = subprocess.Popen(
            command,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                raise RuntimeError("pg-tunnel exited before Jupyter started")
            try:
                api("GET", "/api/status")
                break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.5)
        else:
            raise RuntimeError("Jupyter did not start within 90 seconds")
        runtime = next((root / "runtime").glob("jpserver-*.json"))
        server_pid = json.loads(runtime.read_text())["pid"]
        kernel = api("POST", "/api/kernels", {"name": "recovery-check"})["id"]
        client = BlockingKernelClient(
            connection_file=str(root / "runtime" / f"kernel-{kernel}.json")
        )
        client.load_connection_file()
        client.start_channels()
        client.wait_for_ready(timeout=30)

        def query():
            reply = client.execute_interactive(
                cell, timeout=15, output_hook=lambda message: None
            )
            if reply["content"]["status"] != "ok":
                raise RuntimeError(
                    "fresh notebook connection failed: "
                    + reply["content"].get("ename", "unknown error")
                )
            return json.loads(record.read_text())

        before = query()
        print("Initial read-only TLS login passed.", flush=True)
        if args.active_query:
            reply = client.execute_interactive(
                """
import threading, time
running = psycopg2.connect('', connect_timeout=5)
running.set_session(readonly=True)
active_backend = running.get_backend_pid()
active_result = None
def slow_query():
    global active_result
    try:
        with running.cursor() as cursor:
            cursor.execute("SET statement_timeout = '20s'")
            cursor.execute("SELECT pg_sleep(60)")
        active_result = 'completed'
    except psycopg2.Error as error:
        active_result = type(error).__name__
    finally:
        running.close()
worker = threading.Thread(target=slow_query, daemon=True)
worker.start()
with closing(psycopg2.connect('', connect_timeout=5)) as observer:
    observer.autocommit = True
    with observer.cursor() as cursor:
        for attempt in range(50):
            cursor.execute("SELECT wait_event FROM pg_stat_activity WHERE pid = %s", (active_backend,))
            state = cursor.fetchone()
            if state and state[0] == 'PgSleep':
                break
            time.sleep(0.1)
        else:
            raise RuntimeError('test query did not start')
""",
                timeout=15,
                output_hook=lambda message: None,
            )
            assert reply["content"]["status"] == "ok", "active-query setup failed"
            print(
                "Read-only pg_sleep query confirmed active on the database.", flush=True
            )
        time.sleep(1)  # Let the data channel settle before injecting a fault.
        if args.suspend:
            # Stop only this experiment's process tree; this is not OS sleep.
            os.kill(proc.pid, signal.SIGSTOP)
            suspended.append(proc.pid)
            processes = [
                tuple(map(int, row.split()))
                for row in subprocess.check_output(
                    ["ps", "-axo", "pid=,ppid="], text=True
                ).splitlines()
            ]
            for parent in suspended:
                for pid, ppid in processes:
                    if ppid == parent:
                        os.kill(pid, signal.SIGSTOP)
                        suspended.append(pid)
        interrupted = time.monotonic()
        previous = proxy.interrupt(args.outage)
        print(
            f"Disconnected SSM; blocking new data-channel connections for {args.outage:g}s.",
            flush=True,
        )
        if args.suspend:
            print(
                f"Test processes suspended for {args.suspend:g}s; Mac stays awake.",
                flush=True,
            )
            time.sleep(args.suspend)
            for pid in reversed(suspended):
                os.kill(pid, signal.SIGCONT)
            suspended.clear()
        if args.expect_stop:
            status = proc.wait(timeout=75)
            assert status != 0, "exhausted recovery must exit nonzero"
            assert "SSM recovery exceeded 1m" in (root / "server.log").read_text()
            try:
                os.kill(before["pid"], 0)
            except ProcessLookupError:
                pass
            else:
                raise AssertionError("kernel survived exhausted recovery")
            elapsed = time.monotonic() - interrupted
            assert 55 <= elapsed <= 75, "recovery did not respect its one-minute budget"
            print(
                f"PASS: exhausted recovery reported and kernel stopped after {elapsed:.1f}s.",
                flush=True,
            )
            return
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                raise RuntimeError("pg-tunnel/Jupyter exited during interruption")
            with proxy.lock:
                if proxy.connections > previous and proxy.streams:
                    break
            time.sleep(0.25)
        else:
            api("GET", "/api/status")
            os.kill(before["pid"], 0)
            assert json.loads(runtime.read_text())["pid"] == server_pid
            print(
                f"Jupyter server/kernel survived; proxy rejected {proxy.rejected} reconnect attempts.",
                flush=True,
            )
            raise RuntimeError("no replacement SSM connection within 90 seconds")
        time.sleep(1)  # CONNECT acceptance precedes the encrypted WebSocket handshake.
        after = query()
        if args.active_query:
            reply = client.execute_interactive(
                "worker.join(timeout=25)\n"
                "assert not worker.is_alive(), 'interrupted query stayed blocked'\n"
                "assert active_result in ('QueryCanceled', 'OperationalError'), active_result\n"
                "print('Interrupted query outcome:', active_result)",
                timeout=30,
            )
            assert reply["content"]["status"] == "ok", "active query did not finish"
        assert before["pid"] == after["pid"]
        assert before["backend"] != after["backend"]
        assert (
            before["settings"] == after["settings"] and before["port"] == after["port"]
        )
        assert json.loads(runtime.read_text())["pid"] == server_pid
        api("GET", "/api/status")
        print(
            f"PASS: same server/kernel, port and files; new TLS login after {time.monotonic() - interrupted:.1f}s.",
            flush=True,
        )
    finally:
        for pid in reversed(suspended):
            try:
                os.kill(pid, signal.SIGCONT)
            except ProcessLookupError:
                pass
        if client is not None:
            client.stop_channels()
        if proc is not None and proc.poll() is None:
            try:
                api("POST", "/api/shutdown")
            except (urllib.error.URLError, TimeoutError):
                proc.terminate()
            try:
                proc.wait(timeout=20)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        proxy.shutdown()
        proxy.server_close()
        log.close()
        if record.exists():
            state = json.loads(record.read_text())
            for key in ("PGPASSFILE", "PGSERVICEFILE"):
                assert not Path(state["settings"][key]).exists(), (
                    "private credentials survived shutdown"
                )
            with socket.socket() as probe:
                probe.settimeout(2)
                assert probe.connect_ex(("127.0.0.1", state["port"])) != 0, (
                    "tunnel listener survived shutdown"
                )
            print("Private credential files and tunnel listener removed.", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "connection",
        help="existing pg-tunnel connection with read-only database access",
    )
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument(
        "--executable",
        type=Path,
        default=Path(__file__).resolve().parents[1] / "bin/pg-tunnel",
    )
    parser.add_argument(
        "--outage",
        type=float,
        default=0,
        help="seconds to reject new SSM data connections (0..120)",
    )
    parser.add_argument(
        "--suspend",
        type=float,
        default=0,
        help="suspend only the test process tree for 0..120 seconds (not real laptop sleep)",
    )
    parser.add_argument(
        "--active-query",
        action="store_true",
        help="interrupt a confirmed running read-only query before testing a fresh login",
    )
    parser.add_argument(
        "--expect-stop",
        action="store_true",
        help="require recovery-budget failure and kernel shutdown; use with --outage 75",
    )
    args = parser.parse_args()
    if not 0 <= args.outage <= 120:
        parser.error("--outage must be between 0 and 120 seconds")
    if not 0 <= args.suspend <= 120:
        parser.error("--suspend must be between 0 and 120 seconds")
    if args.suspend and args.expect_stop:
        parser.error("--suspend cannot be combined with --expect-stop")
    if args.expect_stop and args.outage < 75:
        parser.error("--expect-stop requires --outage of at least 75 seconds")
    root = Path(tempfile.mkdtemp(prefix="pg-tunnel-recovery-"))
    versions = {
        name: importlib.metadata.version(name)
        for name in (
            "jupyterlab",
            "jupyter-server",
            "jupyter-client",
            "ipykernel",
            "psycopg2-binary",
        )
    }
    versions.update(
        python=platform.python_version(),
        os=platform.system(),
        architecture=platform.machine(),
    )
    (root / "environment.json").write_text(json.dumps(versions, indent=2))
    print("Private test logs:", root, flush=True)
    try:
        check(args, root)
    except Exception as error:
        print("FAIL:", error, file=sys.stderr)
        sys.exit(1)
