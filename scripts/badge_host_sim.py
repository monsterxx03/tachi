#!/usr/bin/env python3
"""假装成 tachi，给真设备发一条待答项，看它按键后回什么。

它存在的理由：设备侧的按键、界面、回答编码，在没有 tachi 会话（也不想花钱跑一轮）
的时候没法验——而「按下确定，答案真的回得来」恰恰是这块固件唯一要保证的事。

它做三件事：握手（回 state 让设备认为主机在线）、发一条 ask、等 answer。

用法：
    python3 scripts/badge_host_sim.py --port /dev/cu.usbmodem2101
    python3 scripts/badge_host_sim.py --port /dev/cu.usbmodem2101 --kind question
    python3 scripts/badge_host_sim.py --port /dev/cu.usbmodem2101 --text "rm -rf build/"
"""

import argparse
import json
import os
import select
import sys
import termios
import time
import tty

PREFIX = "@@"
HEARTBEAT_REPLY_INTERVAL = 2.0

RESET = "\033[0m"
DIM = "\033[2m"
BOLD = "\033[1m"
GREEN = "\033[32m"


def state_message(seq):
    return {
        "t": "state", "seq": seq,
        "sessions": [{
            "id": "probe", "title": "探针会话", "status": "busy",
            "label": "提问", "detail": "等待回答",
        }],
    }


def permission_message(seq, ref, command):
    return {
        "t": "ask", "seq": seq, "ref": ref, "kind": "permission",
        "session": "probe", "toolId": "call_probe", "title": "Bash 确认",
        "body": f"$ {command}\n（这条来自假主机，不是真的 agent）",
        "options": [
            {"label": "允许一次", "value": "allow_once"},
            {"label": "本会话允许", "value": "allow_session"},
            {"label": "拒绝", "value": "deny"},
        ],
    }


def question_message(seq, ref):
    return {
        "t": "ask", "seq": seq, "ref": ref, "kind": "ask_user",
        "session": "probe", "toolId": "call_probe", "title": "方向",
        "questions": [{
            "header": "方向", "question": "先用哪条路验证？", "multi_select": False,
            "options": [
                {"label": "先做设备", "description": "需要板子"},
                {"label": "先做电脑", "description": "不用板子"},
            ],
        }],
    }


class Host:
    def __init__(self, fd, kind, command):
        self.fd = fd
        self.kind = kind
        self.command = command
        self.buf = b""
        self.seq = 0
        self.ref = 0
        self.ask_sent = False
        self.last_state_at = 0.0

    def send(self, message):
        self.seq += 1
        message["seq"] = self.seq
        os.write(self.fd, (PREFIX + json.dumps(message, ensure_ascii=False) + "\n").encode())
        print(f"{DIM}→ {json.dumps(message, ensure_ascii=False)[:110]}{RESET}")

    def send_state(self, force=False):
        now = time.time()
        if not force and now - self.last_state_at < HEARTBEAT_REPLY_INTERVAL:
            return
        self.last_state_at = now
        self.send(state_message(self.seq))

    def send_ask(self):
        if self.ask_sent:
            return
        self.ask_sent = True
        self.ref += 1
        if self.kind == "question":
            self.send(question_message(self.seq, 100 + self.ref))
        else:
            self.send(permission_message(self.seq, 100 + self.ref, self.command))

    def feed(self, data):
        self.buf += data
        while b"\n" in self.buf:
            raw, self.buf = self.buf.split(b"\n", 1)
            line = raw.rstrip(b"\r")
            if not line.startswith(PREFIX.encode()):
                # 设备的控制台日志：真实主机也会把它丢掉。
                text = line.decode("utf-8", "replace")
                if text.strip():
                    print(f"{DIM}  [设备日志] {text[:100]}{RESET}")
                continue
            try:
                message = json.loads(line[len(PREFIX):].decode())
            except json.JSONDecodeError:
                print(f"{DIM}  [坏行] {line[:80]!r}{RESET}")
                continue
            self.handle(message)

    def handle(self, message):
        kind = message.get("t")
        if kind == "hello":
            print(f"{DIM}  设备握手：{message}{RESET}")
            self.send_state(force=True)
            self.send_ask()
        elif kind == "sync":
            # 设备每 10 秒要一次全量：不回它，30 秒后设备会显示「等电脑」。
            self.send_state()
        elif kind == "answer":
            self.report_answer(message)
        elif kind == "error":
            print(f"  [设备报错] {message.get('message')}")

    def report_answer(self, message):
        ref = message.get("ref")
        print()
        print(f"{GREEN}{BOLD}收到设备的回答{RESET}  ref={ref}")
        print(f"  原始: {json.dumps(message, ensure_ascii=False)}")
        if "decision" in message:
            meaning = {
                "allow_once": "允许这一次",
                "allow_session": "本会话全部允许",
                "deny": "拒绝",
            }.get(message["decision"], "?")
            print(f"  含义: {BOLD}{meaning}{RESET}")
        if "answers" in message:
            for question, answer in message["answers"].items():
                print(f"  {question} → {BOLD}{answer}{RESET}")
        print()
        raise SystemExit(0)


def main():
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--port", default="/dev/cu.usbmodem2101")
    ap.add_argument("--kind", choices=["permission", "question"], default="permission")
    ap.add_argument("--text", default="rm -rf build/", help="permission 模式下命令的内容")
    ap.add_argument("--timeout", type=int, default=180, help="等按键的秒数")
    args = ap.parse_args()

    if not os.path.exists(args.port):
        print(f"串口不存在：{args.port}（设备插上了吗？）")
        return 1

    fd = os.open(args.port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
    try:
        tty.setraw(fd)
        termios.tcflush(fd, termios.TCIFLUSH)
        print(f"{DIM}连上 {args.port}，等设备重启…{RESET}")

        # 不等设备的 hello：它只在启动时发一次，而我们打开串口会把 DTR 拉低让它
        # 重启一次——等我们开始读，那句 hello 早就过去了（上一版就是这么漏掉的，
        # 结果 ask 一直没发出去）。ask 本身是幂等的：设备收到就显示。
        time.sleep(1.5)
        termios.tcflush(fd, termios.TCIFLUSH)

        host = Host(fd, args.kind, args.text)
        host.send_state(force=True)
        host.send_ask()
        deadline = time.time() + args.timeout
        while time.time() < deadline:
            ready, _, _ = select.select([fd], [], [], 0.5)
            if fd in ready:
                try:
                    data = os.read(fd, 4096)
                except OSError:
                    continue
                if data:
                    host.feed(data)
        print(f"\n{DIM}{args.timeout} 秒内没等到回答{RESET}")
        return 2
    finally:
        os.close(fd)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print()
