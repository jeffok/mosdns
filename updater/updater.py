#!/usr/bin/env python3
import os
import socket
import struct
import time
import ssl
from typing import Dict, List, Set, Tuple, Optional

# =========================
# Environment Config
# =========================
ROUTER_HOST = os.getenv("ROS_HOST", "10.100.50.254")
ROUTER_PORT = int(os.getenv("ROS_PORT", "8728"))
ROUTER_USER = os.getenv("ROS_USER", "mosupdater")
ROUTER_PASS = os.getenv("ROS_PASS", "ChangeMeStrong!")

LIST_NAME = os.getenv("ROS_LIST", "chatgpt-sgp")
COMMENT = os.getenv("ROS_COMMENT", "mosdns-chatgpt")

DNS_SERVER = os.getenv("DNS_SERVER", "127.0.0.1")
DNS_PORT = int(os.getenv("DNS_PORT", "53"))

TTL_SECONDS = int(os.getenv("TTL_SECONDS", "1800"))
INTERVAL = int(os.getenv("INTERVAL", "120"))
DOMAINS_FILE = os.getenv("DOMAINS_FILE", "/etc/mosdns/custom-remote.txt")

# TLS verify switches
TLS_VERIFY = os.getenv("TLS_VERIFY", "1").strip() == "1"
TLS_PORT = int(os.getenv("TLS_PORT", "443"))
TLS_TIMEOUT = float(os.getenv("TLS_TIMEOUT", "2.5"))
TLS_CACHE_TTL = int(os.getenv("TLS_CACHE_TTL", "1800"))
TLS_VERIFY_HOST = os.getenv("TLS_VERIFY_HOST", "").strip()  # optional override

# =========================
# Logging
# =========================
def log(msg: str) -> None:
    print(msg, flush=True)

def read_domains(path: str) -> List[str]:
    try:
        with open(path, "r", encoding="utf-8") as f:
            out = []
            for line in f.read().splitlines():
                s = line.strip()
                if not s or s.startswith("#"):
                    continue
                out.append(s)
            return out
    except Exception as e:
        log(f"[ERR] read domains file failed: {path} -> {e}")
        return []

# =========================
# Minimal DNS A query
# =========================
def _read_name(msg: bytes, off: int) -> Tuple[str, int]:
    labels = []
    jumped = False
    joff = off
    while True:
        if off >= len(msg):
            return ("", off)
        l = msg[off]
        if l == 0:
            off += 1
            break
        if (l & 0xC0) == 0xC0:
            if off + 1 >= len(msg):
                return ("", off + 2)
            ptr = ((l & 0x3F) << 8) | msg[off + 1]
            if not jumped:
                joff = off + 2
                jumped = True
            off = ptr
            continue
        off += 1
        if off + l > len(msg):
            return ("", off + l)
        labels.append(msg[off:off + l].decode("utf-8", "ignore"))
        off += l
    return (".".join(labels), (joff if jumped else off))

def dns_query_a(name: str) -> List[str]:
    name = name.strip(".")
    if not name:
        return []
    tid = int(time.time() * 1000) & 0xFFFF
    hdr = struct.pack("!HHHHHH", tid, 0x0100, 1, 0, 0, 0)
    qname = b""
    for p in name.split("."):
        if len(p) > 63:
            return []
        qname += bytes([len(p)]) + p.encode()
    qname += b"\x00"
    pkt = hdr + qname + struct.pack("!HH", 1, 1)

    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(2)
    try:
        s.sendto(pkt, (DNS_SERVER, DNS_PORT))
        msg, _ = s.recvfrom(4096)
    except Exception:
        return []
    finally:
        s.close()

    if len(msg) < 12:
        return []
    rtid, _, qd, an, _, _ = struct.unpack("!HHHHHH", msg[:12])
    if rtid != tid:
        return []

    off = 12
    for _ in range(qd):
        _, off = _read_name(msg, off)
        off += 4

    ips: Set[str] = set()
    for _ in range(an):
        _, off = _read_name(msg, off)
        if off + 10 > len(msg):
            break
        rtype, rclass, _, rdlen = struct.unpack("!HHIH", msg[off:off + 10])
        off += 10
        if off + rdlen > len(msg):
            break
        rdata = msg[off:off + rdlen]
        off += rdlen
        if rtype == 1 and rclass == 1 and rdlen == 4:
            ips.add(socket.inet_ntoa(rdata))
    return sorted(ips)

# =========================
# TLS verify
# =========================
_tls_cache: Dict[Tuple[str, str], Tuple[bool, float]] = {}

def tls_verify_ip_for_domain(ip: str, domain: str) -> bool:
    key = (ip, domain)
    now = time.time()
    if key in _tls_cache:
        ok, exp = _tls_cache[key]
        if now < exp:
            return ok

    ctx = ssl.create_default_context()
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED

    ok = False
    try:
        with socket.create_connection((ip, TLS_PORT), timeout=TLS_TIMEOUT) as raw:
            with ctx.wrap_socket(raw, server_hostname=domain) as ssock:
                ssock.settimeout(TLS_TIMEOUT)
                ssock.version()
                ok = True
    except Exception:
        ok = False

    _tls_cache[key] = (ok, now + TLS_CACHE_TTL)
    return ok

def filter_ips_by_tls(domain_to_ips: Dict[str, List[str]]) -> Set[str]:
    out: Set[str] = set()
    if not TLS_VERIFY:
        for ips in domain_to_ips.values():
            out.update(ips)
        return out

    if TLS_VERIFY_HOST:
        for ips in domain_to_ips.values():
            for ip in ips:
                if tls_verify_ip_for_domain(ip, TLS_VERIFY_HOST):
                    out.add(ip)
        return out

    for dom, ips in domain_to_ips.items():
        for ip in ips:
            if tls_verify_ip_for_domain(ip, dom):
                out.add(ip)
    return out

# =========================
# RouterOS API (robust)
# =========================
def _enc_len(n: int) -> bytes:
    if n < 0x80:
        return bytes([n])
    if n < 0x4000:
        n |= 0x8000
        return bytes([(n >> 8) & 0xFF, n & 0xFF])
    if n < 0x200000:
        n |= 0xC00000
        return bytes([(n >> 16) & 0xFF, (n >> 8) & 0xFF, n & 0xFF])
    if n < 0x10000000:
        n |= 0xE0000000
        return bytes([(n >> 24) & 0xFF, (n >> 16) & 0xFF, (n >> 8) & 0xFF, n & 0xFF])
    return b"\xF0" + struct.pack("!I", n)

def _dec_len(sock: socket.socket) -> int:
    b = sock.recv(1)
    if not b:
        return -1
    c = b[0]
    if (c & 0x80) == 0x00:
        return c
    if (c & 0xC0) == 0x80:
        b2 = sock.recv(1)
        return ((c & 0x3F) << 8) + b2[0]
    if (c & 0xE0) == 0xC0:
        b2 = sock.recv(2)
        return ((c & 0x1F) << 16) + (b2[0] << 8) + b2[1]
    if (c & 0xF0) == 0xE0:
        b2 = sock.recv(3)
        return ((c & 0x0F) << 24) + (b2[0] << 16) + (b2[1] << 8) + b2[2]
    b2 = sock.recv(4)
    return struct.unpack("!I", b2)[0]

def ros_write_sentence(sock: socket.socket, words: List[str]) -> None:
    for w in words:
        wb = w.encode("utf-8")
        sock.sendall(_enc_len(len(wb)) + wb)
    sock.sendall(b"\x00")

def ros_read_sentence(sock: socket.socket) -> List[str]:
    sent = []
    while True:
        l = _dec_len(sock)
        if l < 0:
            return []
        if l == 0:
            break
        sent.append(sock.recv(l).decode("utf-8", "ignore"))
    return sent

def ros_talk(sock: socket.socket, words: List[str]) -> List[List[str]]:
    ros_write_sentence(sock, words)
    rep: List[List[str]] = []
    while True:
        s = ros_read_sentence(sock)
        if not s:
            break
        rep.append(s)
        if s and s[0] == "!done":
            break
    return rep

def ros_login(sock: socket.socket) -> None:
    rep = ros_talk(sock, ["/login", f"=name={ROUTER_USER}", f"=password={ROUTER_PASS}"])
    for s in rep:
        for w in s:
            if w.startswith("=ret="):
                chal = bytes.fromhex(w.split("=", 2)[2])
                import hashlib
                md = hashlib.md5(b"\x00" + ROUTER_PASS.encode() + chal).hexdigest()
                ros_talk(sock, ["/login", f"=name={ROUTER_USER}", f"=response=00{md}"])
                return

def kv_from_sentence(sentence: List[str]) -> Dict[str, str]:
    """
    Parse RouterOS '!re' sentence fields into dict.
    Fields are like:
      '=.id=*A1', '=address=1.2.3.4', '=list=chatgpt-sgp', '=comment=xxx'
    Robustly split only on first '=' after prefix.
    """
    d: Dict[str, str] = {}
    for w in sentence:
        if not w.startswith("="):
            continue
        # strip leading '='
        s = w[1:]
        # split key/value at first '='
        if "=" not in s:
            continue
        k, v = s.split("=", 1)
        d[k] = v
    return d

def get_existing(sock: socket.socket) -> Dict[str, str]:
    rep = ros_talk(sock, ["/ip/firewall/address-list/print", f"?list={LIST_NAME}"])
    m: Dict[str, str] = {}
    for s in rep:
        if not s:
            continue
        if s[0] != "!re":
            continue
        fields = kv_from_sentence(s[1:])
        _id = fields.get(".id") or fields.get("id")  # some versions may vary
        addr = fields.get("address")
        cmt = fields.get("comment")
        if _id and addr and cmt == COMMENT:
            m[addr] = _id
    return m

def ensure(sock: socket.socket, addr: str, existing: Dict[str, str]) -> None:
    timeout = f"{TTL_SECONDS}s"
    if addr in existing:
        ros_talk(sock, [
            "/ip/firewall/address-list/set",
            f"=.id={existing[addr]}",
            f"=timeout={timeout}",
            f"=comment={COMMENT}",
        ])
    else:
        ros_talk(sock, [
            "/ip/firewall/address-list/add",
            f"=list={LIST_NAME}",
            f"=address={addr}",
            f"=timeout={timeout}",
            f"=comment={COMMENT}",
        ])

# =========================
# Main loop
# =========================
def main() -> None:
    log(
        f"[INFO] updater started. DNS={DNS_SERVER}:{DNS_PORT}, "
        f"ROS={ROUTER_HOST}:{ROUTER_PORT}, list={LIST_NAME}, "
        f"tls_verify={TLS_VERIFY}, tls_verify_host={TLS_VERIFY_HOST or '(per-domain)'}"
    )

    while True:
        try:
            domains = read_domains(DOMAINS_FILE)
            if not domains:
                log("[WARN] domains list empty, sleep...")
                time.sleep(INTERVAL)
                continue

            domain_to_ips: Dict[str, List[str]] = {}
            for d in domains:
                ips = dns_query_a(d)
                if ips:
                    domain_to_ips[d] = ips

            if not domain_to_ips:
                log("[WARN] no A records resolved, sleep...")
                time.sleep(INTERVAL)
                continue

            ips_before = set(ip for ips in domain_to_ips.values() for ip in ips)
            ips_ok = filter_ips_by_tls(domain_to_ips)
            if TLS_VERIFY:
                log(f"[INFO] resolved {len(ips_before)} ips, tls-ok {len(ips_ok)} ips")

            if not ips_ok:
                log("[WARN] tls-ok ip set empty, skip writing to ROS this round")
                time.sleep(INTERVAL)
                continue

            sock = socket.create_connection((ROUTER_HOST, ROUTER_PORT), timeout=4)
            sock.settimeout(4)
            try:
                ros_login(sock)
                existing = get_existing(sock)
                for ip in sorted(ips_ok):
                    ensure(sock, ip, existing)
            finally:
                sock.close()

            log(f"[OK] updated {len(ips_ok)} ips to ROS list={LIST_NAME} comment={COMMENT}")

        except Exception as e:
            log(f"[ERR] {e}")

        time.sleep(INTERVAL)

if __name__ == "__main__":
    main()

