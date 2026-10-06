#!/usr/bin/env python3
"""Local-only real PTY viewer for reproducible terminal screenshots.

Download @xterm/xterm 5.5.0 from npm and extract it into
artifacts/e2e/vendor/package, then run:
  python3 scripts/terminal_lab.py -- mesh shell
The browser renders the actual terminal byte stream, also saved as raw ANSI.
This is a development harness, not part of the Mesh service.
"""
import argparse
import base64
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import secrets
import struct
import termios
import threading
import urllib.parse

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--port', type=int, default=8766)
parser.add_argument('--log', default='artifacts/e2e/terminal.ansi')
parser.add_argument('command', nargs=argparse.REMAINDER)
args = parser.parse_args()
command = args.command[1:] if args.command[:1] == ['--'] else args.command
if not command:
    parser.error('a command after -- is required')
root = Path(__file__).resolve().parent.parent
vendor = root / 'artifacts/e2e/vendor/package'
token = secrets.token_urlsafe(32)
buffer = bytearray()
lock = threading.Lock()
pid, master = pty.fork()
if pid == 0:
    os.environ['TERM'] = 'xterm-256color'
    os.environ['COLORTERM'] = 'truecolor'
    os.execvp(command[0], command)
fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 132, 0, 0))
log_path = Path(args.log)
log_path.parent.mkdir(parents=True, exist_ok=True)


def reader():
    with log_path.open('wb') as log:
        while True:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            log.write(data)
            log.flush()
            with lock:
                buffer.extend(data)


threading.Thread(target=reader, daemon=True).start()
html = '''<!doctype html><html><head><meta charset="utf-8">
<title>ai-mesh — real terminal test</title><link rel="stylesheet" href="/xterm.css">
<style>body{margin:0;background:#10151d;color:#dce4ef;font:14px system-ui}
header{padding:16px 22px;border-bottom:1px solid #334155;display:flex;justify-content:space-between}
header span{color:#94a3b8}#terminal{padding:14px 18px}footer{padding:0 22px;color:#94a3b8;font-size:12px}
</style></head><body><header><strong>ai-mesh · terminal verification</strong>
<span>Live PTY · Mac ↔ green-lighthouse · raw ANSI recorded</span></header>
<div id="terminal"></div><footer>Actual CLI output. Ctrl-b, then m: computers · Ctrl-b, then b: return · Ctrl-b, then d: detach</footer>
<script src="/xterm.js"></script><script>
const term = new Terminal({cols:132,rows:38,fontSize:13,fontFamily:'Menlo,monospace',
  theme:{background:'#10151d'},screenReaderMode:true,scrollback:5000});
term.open(document.getElementById('terminal'));term.focus();
const token=TOKEN; let offset=0; let inputQueue=Promise.resolve();
// Browser attachment can occur after tmux's startup-query timeout. Late terminal
// identification replies must not become shell input in this recording harness.
term.onData(data=>{if(/^\x1b\[[?>]?[0-9;]*[cnR]$/.test(data))return;
inputQueue=inputQueue.then(()=>fetch('/input',{method:'POST',headers:{'X-Terminal-Token':token},body:data}));});
async function poll(){try{const res=await fetch('/output?offset='+offset,{headers:{'X-Terminal-Token':token}});
const data=await res.json();offset=data.offset;const bytes=Uint8Array.from(atob(data.data),c=>c.charCodeAt(0));
if(bytes.length)await new Promise(resolve=>term.write(bytes,resolve));}finally{setTimeout(poll,150)}}poll();
</script></body></html>'''.replace('TOKEN', json.dumps(token))


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, data, content_type):
        self.send_response(200)
        self.send_header('Content-Type', content_type)
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.headers.get('Host') != f'127.0.0.1:{args.port}':
            self.send_error(403)
            return
        path = urllib.parse.urlsplit(self.path)
        if path.path == '/':
            self.send(html.encode(), 'text/html; charset=utf-8')
        elif path.path in ('/xterm.js', '/xterm.css'):
            folder = 'lib' if path.path.endswith('.js') else 'css'
            mime = 'text/javascript' if folder == 'lib' else 'text/css'
            self.send((vendor / folder / path.path[1:]).read_bytes(), mime)
        elif path.path == '/output' and self.headers.get('X-Terminal-Token') == token:
            offset = max(0, int(urllib.parse.parse_qs(path.query).get('offset', ['0'])[0]))
            with lock:
                data = bytes(buffer[offset:])
                end = len(buffer)
            self.send(json.dumps({'offset': end, 'data': base64.b64encode(data).decode()}).encode(), 'application/json')
        else:
            self.send_error(404)

    def do_POST(self):
        if (self.path != '/input' or self.headers.get('X-Terminal-Token') != token
                or self.headers.get('Origin') != f'http://127.0.0.1:{args.port}'):
            self.send_error(403)
            return
        size = int(self.headers.get('Content-Length', '0'))
        if size > 65536:
            self.send_error(413)
            return
        try:
            os.write(master, self.rfile.read(size))
        except OSError:
            self.send_error(410)
            return
        self.send(b'{}', 'application/json')


print(f'Live terminal: http://127.0.0.1:{args.port}', flush=True)
try:
    http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Handler).serve_forever()
finally:
    os.close(master)
