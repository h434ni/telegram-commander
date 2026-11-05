#!/usr/bin/env python3
"""
resources.py - Parse and format Linux system resources with emojis and progress bars.
Designed for Telegram Bot output (supports native HTML tags: <b>, <code>).
"""

import argparse
import os
import shutil
import socket
import sys
import time


def make_bar(percent: float, length: int = 10) -> str:
    filled = int(round(length * percent / 100.0))
    filled = max(0, min(length, filled))
    return "■" * filled + "□" * (length - filled)


def format_bytes(bytes_val: float) -> str:
    for unit in ["B", "KB", "MB", "GB", "TB"]:
        if bytes_val < 1024.0 or unit == "TB":
            if unit in ["GB", "TB"]:
                return f"{bytes_val:.1f} {unit}"
            return f"{int(bytes_val)} {unit}"
        bytes_val /= 1024.0
    return f"{bytes_val:.1f} GB"


def get_uptime() -> str:
    try:
        with open("/proc/uptime", "r", encoding="utf-8") as f:
            up_secs = int(float(f.readline().split()[0]))
        days = up_secs // 86400
        hours = (up_secs % 86400) // 3600
        mins = (up_secs % 3600) // 60
        if days > 0:
            return f"{days}d {hours}h {mins}m"
        if hours > 0:
            return f"{hours}h {mins}m"
        return f"{mins}m"
    except Exception:
        return "Unknown"


def get_load_average() -> str:
    try:
        with open("/proc/loadavg", "r", encoding="utf-8") as f:
            parts = f.readline().split()[:3]
            return f"{parts[0]}, {parts[1]}, {parts[2]}"
    except Exception:
        return "Unknown"


def get_cpu_usage(sample_duration: float = 0.15) -> tuple:
    def read_stat():
        with open("/proc/stat", "r", encoding="utf-8") as f:
            line = f.readline()
        fields = [float(x) for x in line.strip().split()[1:]]
        idle = fields[3] + (fields[4] if len(fields) > 4 else 0.0)
        total = sum(fields)
        return idle, total

    try:
        i1, t1 = read_stat()
        time.sleep(sample_duration)
        i2, t2 = read_stat()
        d_idle = i2 - i1
        d_total = t2 - t1
        pct = (1.0 - d_idle / d_total) * 100.0 if d_total > 0 else 0.0
        pct = max(0.0, min(100.0, pct))
    except Exception:
        pct = 0.0

    cores = os.cpu_count() or 1
    return pct, cores


def get_memory_info() -> dict:
    mem = {}
    try:
        with open("/proc/meminfo", "r", encoding="utf-8") as f:
            for line in f:
                parts = line.split(":")
                if len(parts) == 2:
                    key = parts[0].strip()
                    val = int(parts[1].strip().split()[0]) * 1024  # to bytes
                    mem[key] = val
    except Exception:
        pass

    ram_total = mem.get("MemTotal", 0)
    ram_avail = mem.get("MemAvailable", mem.get("MemFree", 0))
    ram_used = max(0, ram_total - ram_avail)
    ram_pct = (ram_used / ram_total * 100.0) if ram_total > 0 else 0.0

    swap_total = mem.get("SwapTotal", 0)
    swap_free = mem.get("SwapFree", 0)
    swap_used = max(0, swap_total - swap_free)
    swap_pct = (swap_used / swap_total * 100.0) if swap_total > 0 else 0.0

    return {
        "ram_total": ram_total,
        "ram_used": ram_used,
        "ram_avail": ram_avail,
        "ram_pct": ram_pct,
        "swap_total": swap_total,
        "swap_used": swap_used,
        "swap_free": swap_free,
        "swap_pct": swap_pct,
    }


def get_disk_info(path: str = "/") -> dict:
    try:
        disk = shutil.disk_usage(path)
        pct = (disk.used / disk.total * 100.0) if disk.total > 0 else 0.0
        return {
            "total": disk.total,
            "used": disk.used,
            "free": disk.free,
            "pct": pct,
        }
    except Exception:
        return {"total": 0, "used": 0, "free": 0, "pct": 0.0}


def main():
    parser = argparse.ArgumentParser(description="Format system resources cleanly for Telegram")
    parser.add_argument("--plain", action="store_true", help="Omit HTML tags (for raw terminal text)")
    args = parser.parse_args()

    hostname = socket.gethostname()
    uptime = get_uptime()
    load_avg = get_load_average()
    cpu_pct, cores = get_cpu_usage(0.15)
    mem = get_memory_info()
    disk = get_disk_info("/")

    if args.plain:
        out = [
            "📊 System Resources",
            f"🖥️ Host: {hostname}",
            f"⏱ Uptime: {uptime}",
            f"📈 Load: {load_avg}",
            "",
            f"🧠 CPU: {cpu_pct:.1f}% ({cores} vCPU)",
            f"[{make_bar(cpu_pct)}]",
            "",
            f"💾 RAM: {format_bytes(mem['ram_used'])} / {format_bytes(mem['ram_total'])} ({mem['ram_pct']:.1f}%)",
            f"[{make_bar(mem['ram_pct'])}] Free: {format_bytes(mem['ram_avail'])}",
            "",
            f"🔄 Swap: {format_bytes(mem['swap_used'])} / {format_bytes(mem['swap_total'])} ({mem['swap_pct']:.1f}%)"
            if mem["swap_total"] > 0 else "🔄 Swap: None",
            f"[{make_bar(mem['swap_pct'])}] Free: {format_bytes(mem['swap_free'])}"
            if mem["swap_total"] > 0 else "",
            "",
            f"💽 Disk (/): {format_bytes(disk['used'])} / {format_bytes(disk['total'])} ({disk['pct']:.1f}%)",
            f"[{make_bar(disk['pct'])}] Free: {format_bytes(disk['free'])}",
        ]
        # Remove empty lines that were conditional
        out = [line for line in out if line != ""]
        print("\n".join(out))
    else:
        swap_line = ""
        if mem["swap_total"] > 0:
            swap_line = (
                f"\n🔄 <b>Swap:</b> {format_bytes(mem['swap_used'])} / {format_bytes(mem['swap_total'])} ({mem['swap_pct']:.1f}%)\n"
                f"<code>[{make_bar(mem['swap_pct'])}]</code> Free: {format_bytes(mem['swap_free'])}"
            )

        msg = (
            f"📊 <b>System Resources</b>\n\n"
            f"🖥️ <b>Host:</b> <code>{hostname}</code>\n"
            f"⏱ <b>Uptime:</b> {uptime}\n"
            f"📈 <b>Load:</b> {load_avg} (1m, 5m, 15m)\n\n"
            f"🧠 <b>CPU:</b> {cpu_pct:.1f}% ({cores} vCPU)\n"
            f"<code>[{make_bar(cpu_pct)}]</code>\n\n"
            f"💾 <b>RAM:</b> {format_bytes(mem['ram_used'])} / {format_bytes(mem['ram_total'])} ({mem['ram_pct']:.1f}%)\n"
            f"<code>[{make_bar(mem['ram_pct'])}]</code> Free: {format_bytes(mem['ram_avail'])}\n"
            f"{swap_line}\n\n"
            f"💽 <b>Disk (/):</b> {format_bytes(disk['used'])} / {format_bytes(disk['total'])} ({disk['pct']:.1f}%)\n"
            f"<code>[{make_bar(disk['pct'])}]</code> Free: {format_bytes(disk['free'])}"
        )
        print(msg)


if __name__ == "__main__":
    main()
