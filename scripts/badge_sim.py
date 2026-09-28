#!/usr/bin/env python3
"""冒充 AI Passport 的假设备：没有板子也能把 badge 链路跑通。

在 tachi desktop 侧打开链路——config.yaml 里写：

    badge:
      transport: tcp
      address: 127.0.0.1:8765

或者用环境变量临时覆盖（覆盖优先级更高）：

    TACHI_BADGE=tcp:127.0.0.1:8765 ./desktop/bin/Tachi.app/Contents/MacOS/Tachi

然后跑这个脚本。它按设备的协议连上来，把收到的东西画成 240x320 那块屏上能看到的
窄幅样子，并把你的按键（数字）当作三键的结果回传。于是不动 ESP-IDF、不烧固件，
就能验证三条路径：状态上报、权限确认、AskUserQuestion。

用法：
    python3 scripts/badge_sim.py                   # 连 127.0.0.1:8765，交互
    python3 scripts/badge_sim.py --pty             # 自己开一对虚拟串口（没有板子时验串口那条路）
    python3 scripts/badge_sim.py --auto deny       # 不交互，权限问题一律回这个决定
    python3 scripts/badge_sim.py --port 9000
"""

import argparse
import json
import os
import select
import socket
import sys
import textwrap

PROTO = 1
PREFIX = "@@"
# 那块屏是 240x320。这里的宽度只是为了让终端里的框看着像它，不是精确换算。
WIDTH = 26

RESET = "\033[0m"
DIM = "\033[2m"
BOLD = "\033[1m"
ACCENT = "\033[36m"
RED = "\033[31m"


def width_of(s):
    """终端里的可见宽度：跳过 ANSI 转义，宽字符算两格。"""
    out = 0
    i = 0
    while i < len(s):
        if s[i] == "\033":
            while i < len(s) and s[i] != "m":
                i += 1
            i += 1
            continue
        out += 2 if ord(s[i]) > 0x2E80 else 1
        i += 1
    return out


def pad(s, width):
    return s + " " * max(0, width - width_of(s))


def wrap(text, indent="  "):
    out = []
    for para in (text or "").splitlines() or [""]:
        para = para.rstrip()
        if not para:
            out.append("")
            continue
        out.extend(textwrap.wrap(para, width=WIDTH - len(indent)) or [""])
    return [indent + line for line in out]


def box(title, body, tail=None):
    """把一屏画成设备那块窄屏的样子。"""
    print()
    print("┌" + "─" * WIDTH + "┐")
    print("│" + pad(BOLD + title + RESET, WIDTH) + "│")
    print("├" + "─" * WIDTH + "┤")
    for line in body:
        print("│" + pad(line, WIDTH) + "│")
    if tail:
        print("├" + "─" * WIDTH + "┤")
        for line in tail:
            print("│" + pad(line, WIDTH) + "│")
    print("└" + "─" * WIDTH + "┘")


class SocketSink:
    """字节出口：一条 TCP 连接。"""

    def __init__(self, sock):
        self.sock = sock

    def send(self, data):
        self.sock.sendall(data)

    def fileno(self):
        return self.sock.fileno()

    def close(self):
        self.sock.close()


class PtySink:
    """字节出口：一对虚拟串口的主端。

    它让「串口」这条路径在没有板子时也能验——真设备上跑的是官方固件，本来就不认识
    我们的协议，所以串口那一侧只能靠假的来试。tachi 打开从端，就像打开真串口一样。
    """

    def __init__(self, fd):
        self.fd = fd

    def send(self, data):
        os.write(self.fd, data)

    def fileno(self):
        return self.fd

    def close(self):
        os.close(self.fd)


class Device:
    def __init__(self, sink, auto=None):
        self.sink = sink
        self.auto = auto
        self.asks = {}      # ref → ask 消息
        self.buf = b""
        self.state = None   # 上一次画过的状态，用来去重

    def send(self, obj):
        self.sink.send((PREFIX + json.dumps(obj, ensure_ascii=False) + "\n").encode())

    def hello(self):
        self.send({"t": "hello", "proto": PROTO, "fw": "sim"})

    def feed(self, data):
        """按行切开收到的字节，处理属于协议的那些。"""
        self.buf += data
        while b"\n" in self.buf:
            raw, self.buf = self.buf.split(b"\n", 1)
            line = raw.rstrip(b"\r")
            if not line.startswith(PREFIX.encode()):
                continue  # 设备上这行会进控制台日志；这里丢掉
            try:
                self.handle(json.loads(line[len(PREFIX):].decode()))
            except json.JSONDecodeError:
                continue

    def handle(self, msg):
        t = msg.get("t")
        if t == "state":
            self.render_state(msg.get("sessions") or [])
        elif t == "ask":
            self.asks[msg["ref"]] = msg
            self.render_ask(msg)
        elif t == "ask_gone":
            if self.asks.pop(msg.get("ref"), None):
                print(f"{DIM}（这一项已被撤掉，不必再答）{RESET}")
        elif t == "error":
            print(f"{RED}！主机报错：{msg.get('message')}{RESET}")

    def render_state(self, sessions):
        if sessions == self.state:
            return
        self.state = sessions
        if not sessions:
            print(f"{DIM}屏幕上：空闲（没有会话在跑）{RESET}")
            return
        for s in sessions:
            title = s.get("title") or s.get("id", "")[:8]
            label = s.get("label") or s.get("status") or ""
            print(f"{DIM}屏幕上：{title} · {label} · {s.get('detail') or ''}{RESET}")

    def render_ask(self, ask):
        if ask.get("kind") == "permission":
            body = wrap(ask.get("body") or "")
            if ask.get("hint"):
                body += wrap(f"（{ask['hint']}）")
            tail = [f"  {i} {opt['label']}" for i, opt in enumerate(ask.get("options") or [], 1)]
            box(ask.get("title") or "确认", body, tail)
        else:
            body = []
            for q in ask.get("questions") or []:
                if q.get("header"):
                    body.append("  " + q["header"])
                body += wrap(q.get("question") or "")
                for i, opt in enumerate(q.get("options") or [], 1):
                    body.append(f"    {i} {opt['label']}")
                body.append("")
            box(ask.get("title") or "问题", body)

        if self.auto is not None:
            self.answer_auto(ask)
        else:
            print(f"{ACCENT}按键 ›{RESET} 数字回车（多个用逗号分隔）", end=" ", flush=True)

    def answer_auto(self, ask):
        ref = ask["ref"]
        if ask.get("kind") == "permission":
            self.send({"t": "answer", "ref": ref, "decision": self.auto})
            print(f"{DIM}（自动回答：{self.auto}）{RESET}")
            return
        answers = {}
        for q in ask.get("questions") or []:
            opts = q.get("options") or []
            if opts:
                answers[q["question"]] = opts[0]["label"]
        self.send({"t": "answer", "ref": ref, "answers": answers, "annotations": {}})
        print(f"{DIM}（自动选了每题的第一个选项）{RESET}")

    def answer(self, text):
        """把终端里的一行输入变成一条回答。"""
        if not self.asks:
            print(f"{DIM}（现在没有问题在等）{RESET}")
            return
        ref = max(self.asks)  # 最新的一条：设备上显示的就是它
        ask = self.asks[ref]
        picks = [p.strip() for p in text.replace("，", ",").split(",") if p.strip()]

        if ask.get("kind") == "permission":
            try:
                choice = (ask.get("options") or [])[int(picks[0]) - 1]
            except (ValueError, IndexError):
                print(f"{RED}！没有这一项{RESET}")
                return
            self.send({"t": "answer", "ref": ref, "decision": choice["value"]})
            print(f"{DIM}→ {choice['label']}{RESET}")
            return

        answers = {}
        for i, q in enumerate(ask.get("questions") or []):
            opts = q.get("options") or []
            pick = picks[i] if i < len(picks) else "1"
            labels = []
            for part in pick.split():
                try:
                    labels.append(opts[int(part) - 1]["label"])
                except (ValueError, IndexError):
                    pass
            if labels:
                answers[q["question"]] = ", ".join(labels)
        self.send({"t": "answer", "ref": ref, "answers": answers, "annotations": {}})
        print(f"{DIM}→ {answers}{RESET}")


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--auto", choices=["allow_once", "allow_session", "deny"],
                    help="不交互：权限问题一律回这个决定")
    ap.add_argument("--pty", action="store_true",
                    help="开一对虚拟串口，等 tachi 用 TACHI_BADGE=serial:<路径> 连上来")
    args = ap.parse_args()

    if args.pty:
        import pty
        import tty

        master, slave = pty.openpty()
        tty.setraw(master)
        path = os.ttyname(slave)
        sink = PtySink(master)
        print(f"{DIM}假设备挂在虚拟串口 {RESET}{BOLD}{path}{RESET}")
        print(f"{DIM}另开一个终端：{RESET}")
        print(f"  TACHI_BADGE=serial:{path} ./desktop/bin/Tachi.app/Contents/MacOS/Tachi")
        print(f"{DIM}（设备和电脑都会以为这是一条真串口；Ctrl-C 退出）{RESET}")
    else:
        sock = socket.create_connection((args.host, args.port))
        sink = SocketSink(sock)
        print(f"{DIM}假设备已连上 {args.host}:{args.port}（Ctrl-C 退出）{RESET}")

    dev = Device(sink, auto=args.auto)
    dev.hello()
    fd = sink.fileno()

    try:
        while True:
            ready, _, _ = select.select([fd, sys.stdin], [], [])
            if fd in ready:
                data = os.read(fd, 65536)
                if not data:
                    print(f"{DIM}主机断开了{RESET}")
                    return
                dev.feed(data)
            if sys.stdin in ready and args.auto is None:
                line = sys.stdin.readline()
                if not line:
                    return
                if line.strip():
                    dev.answer(line.strip())
    except KeyboardInterrupt:
        print()
    finally:
        sink.close()


if __name__ == "__main__":
    main()
