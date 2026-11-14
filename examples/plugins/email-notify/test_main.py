#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""email-notify 的自测：启动一个本机假 SMTP 服务器，按 netPing 的方式（标准输入输出）运行插件。

运行：python test_main.py   （不会向外发送任何邮件）
这也是给自己的插件写测试的参考：构造请求 JSON → 运行插件进程 → 检查退出码和输出。
"""
import json
import os
import socket
import subprocess
import sys
import threading
import unittest

PLUGIN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "main.py")
MARKDOWN = '### 机房网络监控 · 示例机房\n<font color="warning">● 离线</font> **192.168.1.30** 打印机\n>位置：A03'


class FakeSMTP:
    """只接收一封邮件的最小 SMTP 服务器。"""

    def __init__(self):
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(1)
        self.port = self.sock.getsockname()[1]
        self.lines = []
        self.commands = []
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        conn, _ = self.sock.accept()
        f = conn.makefile("rwb")

        def send(text):
            f.write(text.encode() + b"\r\n")
            f.flush()

        send("220 fake")
        in_data = False
        while True:
            raw = f.readline()
            if not raw:
                break
            line = raw.decode("utf-8", "replace").rstrip("\r\n")
            if in_data:
                if line == ".":
                    in_data = False
                    send("250 ok")
                else:
                    self.lines.append(line)
                continue
            self.commands.append(line)
            cmd = line.upper()
            if cmd.startswith(("EHLO", "HELO")):
                send("250 fake")
            elif cmd.startswith("DATA"):
                in_data = True
                send("354 go ahead")
            elif cmd.startswith("QUIT"):
                send("221 bye")
                break
            else:
                send("250 ok")
        conn.close()
        self.sock.close()


def run_plugin(request):
    p = subprocess.run([sys.executable, PLUGIN], input=json.dumps(request).encode("utf-8"),
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    out = p.stdout.decode("utf-8").strip()
    return p.returncode, (json.loads(out) if out else None), p.stderr.decode("utf-8", "replace")


def settings(port, **over):
    s = {"smtp_host": "127.0.0.1", "smtp_port": port, "security": "none", "username": "", "password": "",
         "from_addr": "netping@example.com", "to_addrs": "a@example.com, b@example.com", "subject_prefix": "[netPing]"}
    s.update(over)
    return s


class EmailNotifyTest(unittest.TestCase):
    def test_send(self):
        smtp = FakeSMTP()
        code, resp, _ = run_plugin({
            "api_version": 1, "hook": "notify", "test": True, "settings": settings(smtp.port),
            "payload": {"source": "test", "title": "机房网络监控 · 示例机房", "markdown": MARKDOWN,
                        "text": MARKDOWN.replace('<font color="warning">', "").replace("</font>", "")},
        })
        self.assertEqual(code, 0)
        self.assertEqual(resp, {"ok": True, "message": "已发送给 2 个收件人"})
        mail = "\n".join(smtp.lines)
        self.assertIn("To: a@example.com, b@example.com", mail)
        self.assertIn("Subject: =?utf-8?", mail)
        self.assertIn("text/plain", mail)
        self.assertIn("text/html", mail)
        self.assertEqual(len([c for c in smtp.commands if c.upper().startswith("RCPT TO")]), 2)

    def test_html(self):
        sys.path.insert(0, os.path.dirname(PLUGIN))
        import main  # noqa: PLC0415
        out = main.to_html(MARKDOWN + "\n<script>x</script>")
        self.assertIn("<h3", out)
        self.assertIn('<span style="color:#d9480f">● 离线</span>', out)
        self.assertIn("<b>192.168.1.30</b>", out)
        self.assertIn("&lt;script&gt;", out)  # 其他 HTML 一律转义

    def test_connection_failure(self):
        code, resp, stderr = run_plugin({"hook": "notify", "settings": settings(1), "payload": {}})
        self.assertEqual(code, 1)
        self.assertFalse(resp["ok"])
        self.assertIn("发送邮件失败", resp["error"])
        self.assertIn("SMTP 127.0.0.1:1", stderr)

    def test_missing_settings_and_wrong_hook(self):
        code, resp, _ = run_plugin({"hook": "notify", "settings": settings(25, to_addrs=""), "payload": {}})
        self.assertEqual((code, resp["ok"]), (1, False))
        self.assertIn("收件人", resp["error"])
        code, resp, _ = run_plugin({"hook": "alert", "settings": settings(25), "payload": {}})
        self.assertEqual((code, resp["ok"]), (1, False))


if __name__ == "__main__":
    unittest.main()
