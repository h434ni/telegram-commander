#!/usr/bin/env python3
"""
send_pm2_rich_table.py
Fetches PM2 process status via `pm2 jlist` and sends it as a native rich table
using Telegram Bot API's `sendRichMessage` with HTML <table> support (Bot API 10.2+).
"""

import argparse
import datetime
import html
import json
import os
import socket
import subprocess
import sys
import urllib.error
import urllib.request
import yaml


def format_uptime(ms_timestamp):
    if not ms_timestamp:
        return "-"
    now = datetime.datetime.now(datetime.timezone.utc).timestamp()
    delta = max(0, int(now - (ms_timestamp / 1000.0)))
    days = delta // 86400
    hours = (delta % 86400) // 3600
    mins = (delta % 3600) // 60
    if days > 0:
        return f"{days}d{hours}h"
    if hours > 0:
        return f"{hours}h{mins}m"
    return f"{mins}m"


def load_config(config_path):
    if not os.path.isfile(config_path):
        return None, []
    try:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = yaml.safe_load(f) or {}
            tg = cfg.get("telegram", {})
            token = tg.get("bot_token")
            allowed = tg.get("allowed_users", [])
            return token, allowed
    except Exception as e:
        print(f"Warning: Failed to parse config {config_path}: {e}", file=sys.stderr)
        return None, []


def send_telegram_rich_message(bot_token, chat_id, html_content):
    url = f"https://api.telegram.org/bot{bot_token}/sendRichMessage"
    payload = json.dumps({
        "chat_id": chat_id,
        "rich_message": {
            "html": html_content.strip()
        }
    }).encode("utf-8")

    req = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        res = json.loads(resp.read().decode("utf-8"))
        if not res.get("ok"):
            raise RuntimeError(f"sendRichMessage error: {res}")
        return res.get("result", {})


def send_telegram_legacy_fallback(bot_token, chat_id, text):
    url = f"https://api.telegram.org/bot{bot_token}/sendMessage"
    payload = json.dumps({
        "chat_id": chat_id,
        "text": text,
        "parse_mode": "HTML"
    }).encode("utf-8")

    req = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode("utf-8"))


def main():
    parser = argparse.ArgumentParser(description="Send PM2 status as native Telegram rich table")
    parser.add_argument("--config", default="/home/ec2-user/apps/telegram-commander/config.yaml",
                        help="Path to telegram-commander config.yaml")
    parser.add_argument("--token", default=os.environ.get("TG_BOT_TOKEN"),
                        help="Telegram bot token")
    parser.add_argument("--chat-id", default=os.environ.get("TG_CHAT_ID"),
                        help="Specific Telegram chat ID to send to")
    args = parser.parse_args()

    cfg_token, cfg_allowed = load_config(args.config)
    bot_token = args.token or cfg_token
    if not bot_token:
        print("Error: No bot token configured.", file=sys.stderr)
        sys.exit(1)

    recipients = []
    if args.chat_id:
        recipients.append(args.chat_id)
    elif cfg_allowed:
        recipients.extend(cfg_allowed)
    else:
        print("Error: No recipients/chat_id found in arguments or config.", file=sys.stderr)
        sys.exit(1)

    # Fetch PM2 process list
    try:
        res = subprocess.run(["pm2", "jlist"], capture_output=True, text=True, check=True)
        procs = json.loads(res.stdout)
    except Exception as e:
        print(f"Error fetching PM2 status: {e}", file=sys.stderr)
        sys.exit(1)

    hostname = html.escape(socket.gethostname())
    now_utc = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")

    if not procs:
        empty_html = (
            f"<h3>⚡ PM2 Process Monitor</h3>"
            f"<p>🖥️ Host: <code>{hostname}</code> | 🕒 <code>{now_utc} UTC</code></p>"
            f"<p><i>No active processes managed by PM2.</i></p>"
        )
        for cid in recipients:
            try:
                send_telegram_rich_message(bot_token, cid, empty_html)
            except Exception:
                send_telegram_legacy_fallback(bot_token, cid, empty_html)
        print("No processes running. Sent notification.")
        return

    # Build Native HTML Table
    table_rows = []
    total_mem = 0.0
    total_cpu = 0.0
    online_count = 0
    stopped_count = 0
    errored_count = 0

    for p in procs:
        pm_id = html.escape(str(p.get("pm_id", "")))
        name = html.escape(p.get("name", "unknown"))

        raw_status = p.get("pm2_env", {}).get("status", "unknown")
        if raw_status == "online":
            online_count += 1
            status_cell = "🟢 online"
        elif raw_status in ("stopped", "stopping"):
            stopped_count += 1
            status_cell = "🔴 stopped"
        else:
            errored_count += 1
            status_cell = f"⚠️ {html.escape(raw_status)}"

        cpu = p.get("monit", {}).get("cpu", 0)
        total_cpu += cpu
        cpu_str = f"{cpu:.1f}%"

        mem_bytes = p.get("monit", {}).get("memory", 0)
        mem_mb = mem_bytes / (1024 * 1024)
        total_mem += mem_mb
        mem_str = f"{mem_mb:.1f}M"

        uptime_str = format_uptime(p.get("pm2_env", {}).get("pm_uptime"))
        restarts = str(p.get("pm2_env", {}).get("restart_time", 0))

        row_html = (
            f"  <tr>\n"
            f'    <td align="center">{pm_id}</td>\n'
            f'    <td align="left"><b>{name}</b></td>\n'
            f'    <td align="center">{status_cell}</td>\n'
            f'    <td align="right">{cpu_str}</td>\n'
            f'    <td align="right">{mem_str}</td>\n'
            f'    <td align="center">{uptime_str}</td>\n'
            f'    <td align="center">{restarts}</td>\n'
            f"  </tr>"
        )
        table_rows.append(row_html)

    rows_combined = "\n".join(table_rows)

    summary_extra = []
    if stopped_count > 0:
        summary_extra.append(f"🔴 Stopped: <b>{stopped_count}</b>")
    if errored_count > 0:
        summary_extra.append(f"⚠️ Errored: <b>{errored_count}</b>")
    extra_str = (" | " + " | ".join(summary_extra)) if summary_extra else ""

    rich_html = f"""
<h3>⚡ PM2 Process Monitor</h3>
<p>🖥️ Host: <code>{hostname}</code> | 🕒 <code>{now_utc} UTC</code></p>
<table bordered striped compact>
  <tr>
    <th align="center">ID</th>
    <th align="left">Process</th>
    <th align="center">Status</th>
    <th align="right">CPU</th>
    <th align="right">Mem</th>
    <th align="center">Uptime</th>
    <th align="center">↺</th>
  </tr>
{rows_combined}
</table>
<p><b>Summary:</b> Total: <b>{len(procs)}</b> | 🟢 Online: <b>{online_count}</b>{extra_str} | 🧠 CPU: <b>{total_cpu:.1f}%</b> | 🧮 RAM: <b>{total_mem:.1f} MB</b></p>
"""

    sent_count = 0
    for cid in recipients:
        try:
            send_telegram_rich_message(bot_token, cid, rich_html)
            sent_count += 1
        except Exception as err:
            print(f"sendRichMessage failed for {cid} ({err}), falling back to text...", file=sys.stderr)
            fallback_text = (
                f"<b>⚡ PM2 Process Monitor</b>\n"
                f"🖥️ Host: <code>{hostname}</code> | 🕒 <code>{now_utc} UTC</code>\n\n"
                f"<pre>Total: {len(procs)} | Online: {online_count} | RAM: {total_mem:.1f}MB</pre>"
            )
            try:
                send_telegram_legacy_fallback(bot_token, cid, fallback_text)
                sent_count += 1
            except Exception as ferr:
                print(f"Fallback also failed for {cid}: {ferr}", file=sys.stderr)

    print(f"Delivered native PM2 rich table ({len(procs)} processes) to {sent_count} Telegram chat(s).")


if __name__ == "__main__":
    main()
