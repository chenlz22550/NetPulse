#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""netPing 示例插件：邮件通知（notify 钩子）。

netPing 把一个 JSON 请求写到标准输入，本程序发送邮件后把一个 JSON 响应写到标准输出。
只用 Python 标准库，无需安装第三方包。
"""
import html
import json
import re
import smtplib
import ssl
import sys
from email.header import Header
from email.mime.multipart import MIMEMultipart
from email.mime.text import MIMEText
from email.utils import formataddr, formatdate


def reply(ok, message="", error=""):
    """按协议输出响应并退出。失败时退出码非 0，netPing 会记为发送失败。"""
    out = {"ok": ok}
    if message:
        out["message"] = message
    if error:
        out["error"] = error
    sys.stdout.write(json.dumps(out, ensure_ascii=False) + "\n")
    sys.exit(0 if ok else 1)


def to_html(markdown):
    """把企业微信风格的 markdown 转成简单的 HTML（标题、加粗、引用、颜色）。"""
    colors = {"warning": "#d9480f", "info": "#2b8a3e", "comment": "#868e96"}
    lines = []
    for raw in markdown.split("\n"):
        line = html.escape(raw, quote=False)
        line = re.sub(r'&lt;font color="?(\w+)"?&gt;', lambda m: '<span style="color:%s">' % colors.get(m.group(1), "inherit"), line)
        line = line.replace("&lt;/font&gt;", "</span>")
        line = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", line)
        if line.startswith("### "):
            line = "<h3 style=\"margin:0 0 8px\">%s</h3>" % line[4:]
        elif line.startswith("&gt;"):
            line = "<div style=\"color:#495057;padding-left:10px;border-left:3px solid #dee2e6\">%s</div>" % line[4:]
        else:
            line = "<div>%s</div>" % line
        lines.append(line)
    return "<div style=\"font:14px/1.7 sans-serif\">%s</div>" % "".join(lines)


def main():
    try:
        # 明确按 UTF-8 读取，避免中文 Windows 默认编码导致乱码
        req = json.loads(sys.stdin.buffer.read().decode("utf-8"))
    except Exception as e:  # noqa: BLE001
        reply(False, error="请求不是有效的 JSON: %s" % e)
    if req.get("hook") != "notify":
        reply(False, error="本插件只处理 notify 钩子，收到 %s" % req.get("hook"))

    s = req.get("settings") or {}
    payload = req.get("payload") or {}
    host = (s.get("smtp_host") or "").strip()
    port = int(s.get("smtp_port") or 465)
    security = s.get("security") or "ssl"
    user = (s.get("username") or "").strip()
    password = s.get("password") or ""
    sender = (s.get("from_addr") or "").strip() or user
    to = [x.strip() for x in re.split(r"[,;\s]+", s.get("to_addrs") or "") if x.strip()]
    if not host or not to:
        reply(False, error="请先在「插件」页填写 SMTP 服务器和收件人")
    if not sender:
        reply(False, error="请填写发件人地址或登录账号")

    title = payload.get("title") or "netPing 通知"
    subject = ("%s %s" % ((s.get("subject_prefix") or "").strip(), title)).strip()
    if req.get("test"):
        subject += "（测试）"
    msg = MIMEMultipart("alternative")
    msg["Subject"] = Header(subject, "utf-8")
    msg["From"] = formataddr((str(Header("netPing", "utf-8")), sender))
    msg["To"] = ", ".join(to)
    msg["Date"] = formatdate(localtime=True)
    msg.attach(MIMEText(payload.get("text") or "", "plain", "utf-8"))
    msg.attach(MIMEText(to_html(payload.get("markdown") or payload.get("text") or ""), "html", "utf-8"))

    try:
        if security == "ssl":
            server = smtplib.SMTP_SSL(host, port, timeout=20, context=ssl.create_default_context())
        else:
            server = smtplib.SMTP(host, port, timeout=20)
        with server:
            if security == "starttls":
                server.starttls(context=ssl.create_default_context())
            if user:
                server.login(user, password)
            refused = server.sendmail(sender, to, msg.as_string())
    except Exception as e:  # noqa: BLE001
        # 调试信息写到标准错误，netPing 会记到日志和「最近调用」里
        sys.stderr.write("SMTP %s:%s (%s) 失败: %r\n" % (host, port, security, e))
        reply(False, error="发送邮件失败: %s" % e)
    if refused:
        reply(False, error="部分收件人被拒绝: %s" % ", ".join(refused))
    reply(True, message="已发送给 %d 个收件人" % len(to))


if __name__ == "__main__":
    main()
