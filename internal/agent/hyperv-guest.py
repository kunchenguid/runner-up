#!/usr/bin/env python3
# runner-up's program inside a Linux guest: a Hyper-V place's VM or an Apple
# container place's guest. The agent fills in the @...@ values.
#
# In a Hyper-V VM, cloud-init writes it to /opt/runner-up/guest.py and runs it
# as root once the VM has booted. It talks to the agent over the VM's second
# serial port (COM 2, /dev/ttyS1): it says it is ready until the agent sends
# the runner's one-time registration on one line, and turns the VM off at the
# end.
#
# In an Apple container guest (the argument stdin), the agent's bootstrap
# writes it to the same path and runs it as root; the registration arrives on
# one line of standard input, output goes to standard output, and the guest
# stops when the program ends.
#
# Either way it downloads GitHub's runner, checks it against its pin and runs
# it as the user runner for one job. The registration goes into the runner's
# environment only, never into a file or an argument. The runner also gets
# what the image sets in /etc/environment, as a login would, such as
# RUNNER_TOOL_CACHE for tools the image carries, as GitHub's Ubuntu images set
# it.
#
# With the argument guard it is instead the runner's fork guard, the same
# decision as runner-up's own fork-guard command (forkguard.go), since the
# agent's own program cannot run in a Linux guest.
import hashlib
import json
import os
import select
import shutil
import subprocess
import sys
import time
import tty
import urllib.request

PORT = "/dev/ttyS1"
RUNNER_URL = "@RUNNER_URL@"
RUNNER_SHA256 = "@RUNNER_SHA256@"
# The runner's launch, as runner-up decides it for every place (launch.go):
# its variables, and the fork guard's hook file and content, empty when the
# place allows forks' pull requests.
LAUNCH = @LAUNCH@
HOME = "/home/runner"
READY = b"runner-up guest: ready\n"


def lookup(v, *path):
    for k in path:
        if not isinstance(v, dict):
            return None
        v = v.get(k)
    return v


def same_repo(head, base):
    def plain(n):
        return isinstance(n, int) and not isinstance(n, bool) and -(2**63) <= n < 2**63

    return plain(head) and plain(base) and head > 0 and head == base


def name_or(v, fallback):
    return v if isinstance(v, str) and v else fallback


def reject(constant):
    # JSON has no NaN or Infinity; Go's reader refuses them too.
    raise ValueError(constant)


def fork_guard(path):
    """None allows the job; a reason refuses it."""
    if not path:
        return "no event file"
    try:
        with open(path, "rb") as f:
            text = f.read().decode("utf-8", errors="replace")
    except OSError:
        return "no event file"
    try:
        event = json.loads(text, parse_constant=reject)
    except ValueError:
        return "unreadable event file"
    if not isinstance(event, dict):
        return "unreadable event file"
    pr = event.get("pull_request")
    if pr is not None:
        if not same_repo(lookup(pr, "head", "repo", "id"), lookup(pr, "base", "repo", "id")):
            return "pull request from " + name_or(lookup(pr, "head", "repo", "full_name"), "a deleted fork")
    run = event.get("workflow_run")
    if run is not None:
        if not same_repo(lookup(run, "head_repository", "id"), lookup(run, "repository", "id")):
            return "follow-on run of " + name_or(lookup(run, "head_repository", "full_name"), "a deleted fork")
    return None


def run_guard():
    reason = fork_guard(os.environ.get("GITHUB_EVENT_PATH", ""))
    if reason:
        sys.stderr.write(
            "runner-up refuses this job: %s. Jobs from another repository's pull request never run on these runners.\n"
            % reason
        )
        return 1
    print("runner-up: %s job from this repository; allowed" % (os.environ.get("GITHUB_EVENT_NAME") or "event"))
    return 0


def registration(fd, announce=True):
    """Reads the agent's first line from fd; announcing, says ready on fd
    every two seconds until the line comes."""
    got = b""
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        if announce:
            os.write(fd, READY)
        until = time.monotonic() + 2
        while time.monotonic() < until:
            r, _, _ = select.select([fd], [], [], max(until - time.monotonic(), 0))
            if not r:
                continue
            chunk = os.read(fd, 65536)
            if not chunk:
                raise RuntimeError("the agent closed its input before the registration")
            got += chunk
            if b"\n" in got:
                return got.split(b"\n", 1)[0].strip().decode()
    raise RuntimeError("no registration from the agent")


def prepare():
    tarball = os.path.join(HOME, "runner.tar.gz")
    h = hashlib.sha256()
    with urllib.request.urlopen(RUNNER_URL, timeout=300) as r, open(tarball, "wb") as f:
        while chunk := r.read(1 << 20):
            h.update(chunk)
            f.write(chunk)
    if h.hexdigest() != RUNNER_SHA256:
        raise RuntimeError("the runner's download does not match its pin")
    runner = os.path.join(HOME, "actions-runner")
    shutil.rmtree(runner, ignore_errors=True)
    os.makedirs(runner)
    subprocess.run(["tar", "xzf", tarball, "-C", runner], check=True)
    os.remove(tarball)
    subprocess.run(["chown", "-R", "runner:runner", runner], check=True)
    if LAUNCH["hook"]:
        with open(LAUNCH["hook"], "w") as f:
            f.write(LAUNCH["guard"])
        os.chmod(LAUNCH["hook"], 0o755)
    return runner


def image_environment(path="/etc/environment"):
    """The variables a file like /etc/environment sets: NAME=value lines,
    the value unquoted, as pam_env reads them."""
    env = {}
    try:
        with open(path) as f:
            lines = f.read().splitlines()
    except OSError:
        return env
    for line in lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        if line.startswith("export "):
            line = line[len("export "):].lstrip()
        name, value = line.split("=", 1)
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        env[name.strip()] = value
    return env


def runner_env(jit, image=None):
    env = {
        "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin",
    }
    env.update({k: v for k, v in (image or {}).items() if not k.startswith("ACTIONS_RUNNER_")})
    env.update({
        "HOME": HOME,
        "USER": "runner",
        "LOGNAME": "runner",
        "SHELL": "/bin/bash",
        "LANG": env.get("LANG", "C.UTF-8"),
    })
    env.update(LAUNCH["env"])
    env["ACTIONS_RUNNER_INPUT_JITCONFIG"] = jit
    return env


def main():
    if sys.argv[1:] == ["guard"]:
        return run_guard()
    container = sys.argv[1:] == ["stdin"]
    if container:
        rfd, wfd = 0, 1
    else:
        rfd = wfd = os.open(PORT, os.O_RDWR | os.O_NOCTTY)
        tty.setraw(rfd)

    def say(msg):
        os.write(wfd, ("runner-up guest: %s\n" % msg).encode())

    try:
        jit = registration(rfd, announce=not container)
        runner = prepare()
        env = runner_env(jit, image_environment())
        del jit
        say("starting the runner")
        code = subprocess.run(
            [os.path.join(runner, "run.sh")],
            cwd=runner,
            env=env,
            user="runner",
            group="runner",
            extra_groups=[],
            stdin=subprocess.DEVNULL,
            stdout=wfd,
            stderr=subprocess.STDOUT,
        ).returncode
        say("the runner ended with %d" % code)
    except Exception as e:
        say("failed: %s" % e)
        # A container guest's exit status is the agent's only sign that no
        # runner ran; a VM turns off either way.
        if container:
            return 1
    finally:
        if not container:
            subprocess.run(["systemctl", "poweroff"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
