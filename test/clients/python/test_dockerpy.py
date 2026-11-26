"""Docker's Python SDK, docker-py, against runwhp's Engine API.

test/e2e runs it with the venv's python, which has requirements.txt, and
DOCKER_HOST naming a runwhp serving a store of the test's own, or with
no DOCKER_HOST and a DOCKER_CONFIG whose current context names it. It
prints each failure and exits 1 if there was any.
"""

import os
import socket
import sys
import tempfile
import time

import docker

ALPINE = "alpine:3.20"
failures = []


def check(cond, what):
    if not cond:
        failures.append(what)
        print("FAIL:", what, flush=True)


def main():
    client = docker.from_env()
    version = client.version()
    check(version.get("Platform", {}).get("Name") == "runwhp", "version: %r" % version.get("Platform"))
    check(client.ping(), "ping")
    if len(sys.argv) > 1 and sys.argv[1] == "context":
        # Only the context was there to find it.
        check("DOCKER_HOST" not in os.environ, "DOCKER_HOST set for the context run")
        return

    # A run that is not detached gives its output, which docker-py reads
    # only from a json-file or journald log.
    out = client.containers.run(ALPINE, ["sh", "-c", "echo hi"], remove=True)
    check(out == b"hi\n", "run's output: %r" % out)

    c = client.containers.run(ALPINE, ["nc", "-lk", "-p", "80", "-e", "cat", "/etc/hostname"], detach=True,
                              ports={"80/tcp": None}, labels={"py": "yes"})
    try:
        c.reload()
        check(c.status == "running", "status %r" % c.status)
        host_port = c.ports["80/tcp"][0]["HostPort"]
        check(host_port not in ("", "0"), "ports %r" % c.ports)
        body = b""
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", int(host_port)), timeout=2) as s:
                    s.settimeout(5)
                    body = s.makefile("rb").read()
                if body:
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check(body.strip() != b"", "nothing served on the random port %s" % host_port)

        r = c.exec_run(["sh", "-c", "echo out; exit 2"])
        check(r.exit_code == 2 and r.output == b"out\n", "exec_run: %r" % (r,))
        top = c.top()
        check("PID" in top["Titles"] and any("nc -lk" in " ".join(p) for p in top["Processes"]), "top: %r" % top)
        check([x.id for x in client.containers.list(filters={"label": "py=yes"})] == [c.id], "list by label")
        c.stop(timeout=1)
        check(c.wait()["StatusCode"] in (0, 137, 143), "wait")
    finally:
        c.remove(force=True)

    with tempfile.TemporaryDirectory() as ctx:
        with open(os.path.join(ctx, "Dockerfile"), "w") as f:
            f.write("FROM %s\nRUN echo built > /built\n" % ALPINE)
        img, _ = client.images.build(path=ctx, tag="dockerpy/built:1", rm=True)
        check("dockerpy/built:1" in img.tags, "built image tags %r" % img.tags)
        out = client.containers.run("dockerpy/built:1", ["cat", "/built"], remove=True)
        check(out == b"built\n", "the built image's file: %r" % out)
        client.images.remove("dockerpy/built:1")

    net = client.networks.create("pynet", labels={"py": "yes"})
    vol = client.volumes.create("pyvol")
    try:
        server = client.containers.run(ALPINE, ["sh", "-c", "echo kept > /data/f; exec nc -lk -p 80 -e cat /data/f"],
                                       detach=True, network="pynet", name="pyweb", volumes={"pyvol": {"bind": "/data"}})
        try:
            out = b""
            deadline = time.time() + 20
            while time.time() < deadline and out != b"kept\n":
                out = client.containers.run(ALPINE, ["sh", "-c", "nc pyweb 80 || true"],
                                            network="pynet", remove=True)
                if out != b"kept\n":
                    time.sleep(0.2)
            check(out == b"kept\n", "a container by name on a network: %r" % out)
        finally:
            server.remove(force=True)
        check(any(n.name == "pynet" for n in client.networks.list(filters={"label": "py=yes"})), "networks.list")
    finally:
        net.remove()
        vol.remove()


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # what docker-py raised is the failure
        failures.append(repr(e))
        print("FAIL:", repr(e), flush=True)
    sys.exit(1 if failures else 0)
