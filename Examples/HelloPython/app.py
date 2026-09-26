#!/usr/bin/env python3
"""
Simple Hello World Python HTTP Server
"""


from http.server import HTTPServer, BaseHTTPRequestHandler
import json
import os
import socket

class HelloWorldHandler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def send_body(self, status, content_type, body):
        payload = body.encode('utf-8')
        self.send_response(status)
        self.send_header('Content-type', content_type)
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path == '/':
            html_content = """
            <!doctype html>
            <html lang="en">
            <head>
                <meta charset="utf-8">
                <meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
                <meta name="theme-color" content="#f1eee7">
                <title>Hello Python · Wendy</title>
                <style>
                    :root {
                        --cream:#f1eee7; --slate:#171c23; --muted:#5b5a56;
                        --border:#d7d5cf; --seafoam:#9fe2bf; --green:#2a7050;
                        font-family:"Geist",ui-sans-serif,system-ui,-apple-system,sans-serif;
                        background:var(--cream); color:var(--slate);
                    }
                    * { box-sizing:border-box; }
                    body { margin:0; min-height:100vh; }
                    main { margin:auto; max-width:1040px; padding:clamp(28px,6vw,80px); }
                    .brand { align-items:center; border-bottom:1px solid var(--border); display:flex; gap:14px; padding-bottom:28px; }
                    .mark { height:34px; position:relative; transform:rotate(45deg); width:34px; }
                    .mark::before { border:6px solid var(--slate); content:""; inset:0; position:absolute; }
                    .mark::after { background:var(--slate); content:""; height:22px; left:18px; position:absolute; top:-6px; width:22px; }
                    .brand strong { font-size:18px; letter-spacing:.08em; }
                    .eyebrow { color:var(--green); font:600 12px/1.2 ui-monospace,SFMono-Regular,monospace; letter-spacing:.16em; text-transform:uppercase; }
                    h1 { font-size:clamp(52px,9vw,104px); font-weight:500; letter-spacing:-.06em; line-height:.92; margin:72px 0 20px; }
                    .lede { color:var(--muted); font-size:clamp(18px,2vw,24px); line-height:1.45; max-width:680px; }
                    .status { align-items:center; border-bottom:1px solid var(--border); border-top:1px solid var(--border); display:grid; gap:20px; grid-template-columns:1fr auto; margin-top:64px; padding:24px 0; }
                    .status strong { color:var(--green); font-size:18px; }
                    .status strong::before { background:var(--green); border-radius:50%; content:""; display:inline-block; height:9px; margin-right:10px; width:9px; }
                    .links { display:flex; flex-wrap:wrap; gap:10px; }
                    a { background:var(--seafoam); color:var(--slate); font-weight:650; min-height:48px; padding:13px 17px; text-decoration:none; }
                    a.secondary { background:transparent; border:1px solid var(--border); }
                    a.home { background:transparent; border:1px solid var(--border); margin-left:auto; }
                    @media(max-width:640px) {
                        main { padding:24px; }
                        h1 { margin-top:54px; }
                        .status { align-items:start; grid-template-columns:1fr; }
                    }
                </style>
            </head>
            <body>
                <main>
                    <header class="brand"><span class="mark" aria-hidden="true"></span><strong>WENDY</strong><a class="home" href="http://127.0.0.1:8088/">← Home</a></header>
                    <p class="eyebrow">WendyOS example · Python</p>
                    <h1>Hello,<br>edge.</h1>
                    <p class="lede">A small Python HTTP service running locally on a WendyOS device.</p>
                    <section class="status" aria-label="Service status">
                        <strong>Service online</strong>
                        <nav class="links"><a href="/api/hello">Open JSON API ↗</a><a class="secondary" href="/health">Health check</a></nav>
                    </section>
                </main>
            </body>
            </html>
            """
            self.send_body(200, 'text/html', html_content)
            
        elif self.path == '/api/hello':
            response = {
                "message": "Hello World!",
                "status": "success",
                "server": "Python HTTP Server"
            }
            self.send_body(200, 'application/json', json.dumps(response, indent=2))
            
        elif self.path == '/health':
            health_response = {
                "status": "healthy",
                "message": "Server is running"
            }
            self.send_body(200, 'application/json', json.dumps(health_response))
            
        else:
            error_response = {
                "error": "Not Found",
                "message": f"Path {self.path} not found"
            }
            self.send_body(404, 'application/json', json.dumps(error_response))

    def do_POST(self):
        if self.path == '/api/echo':
            content_length = int(self.headers['Content-Length'])
            post_data = self.rfile.read(content_length)
            
            response = {
                "message": "Echo endpoint",
                "received_data": post_data.decode('utf-8'),
                "status": "success"
            }
            self.send_body(200, 'application/json', json.dumps(response, indent=2))
        else:
            error_response = {
                "error": "Not Found",
                "message": f"POST endpoint {self.path} not found"
            }
            self.send_body(404, 'application/json', json.dumps(error_response))

    def log_message(self, format, *args):
        print(f"[{self.date_time_string()}] {format % args}", flush=True)

def run_server(port=8000):
    server_address = ('0.0.0.0', port)
    httpd = HTTPServer(server_address, HelloWorldHandler)
    print(f"Starting server on {server_address[0]}:{server_address[1]}", flush=True)
    print(f"Visit http://localhost:{port} to see the Hello World page", flush=True)
    print(f"API endpoints available:", flush=True)
    print(f"  GET  /api/hello - JSON hello message", flush=True)
    print(f"  GET  /health - Health check", flush=True)
    print(f"  POST /api/echo - Echo back posted data", flush=True)
    print("Server is ready to accept connections...", flush=True)
    
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down server...", flush=True)
        httpd.shutdown()

if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8000))
    run_server(port)
