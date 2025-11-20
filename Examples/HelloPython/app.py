#!/usr/bin/env python3
"""
Simple Hello World Python HTTP Server
"""

from http.server import HTTPServer, BaseHTTPRequestHandler
import json
import os
import socket

class HelloWorldHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/':
            self.send_response(200)
            self.send_header('Content-type', 'text/html')
            self.end_headers()
            
            html_content = """
            <!DOCTYPE html charset="utf-8">
            <html lang="en">
            <head>
                <title>Hello World Server</title>
                <style>
                    body { 
                        font-family: Arial, sans-serif; 
                        max-width: 800px; 
                        margin: 50px auto; 
                        padding: 20px;
                        background-color: #f5f5f5;
                    }
                    .container {
                        background-color: white;
                        padding: 30px;
                        border-radius: 10px;
                        box-shadow: 0 2px 10px rgba(0,0,0,0.1);
                        text-align: center;
                    }
                    h1 { color: #333; }
                    .status { color: #28a745; font-weight: bold; }
                </style>
            </head>
            <body>
                <div class="container">
                    <h1>Hello World</h1>
                    <p class="status">Server is running successfully!</p>
                    <p>This is a simple Python HTTP server running in a Docker container.</p>
                </div>
            </body>
            </html>
            """
            self.wfile.write(html_content.encode())
            
        elif self.path == '/api/hello':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            response = {
                "message": "Hello World!",
                "status": "success",
                "server": "Python HTTP Server"
            }
            self.wfile.write(json.dumps(response, indent=2).encode())
            
        elif self.path == '/health':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            health_response = {
                "status": "healthy",
                "message": "Server is running"
            }
            self.wfile.write(json.dumps(health_response).encode())
            
        else:
            self.send_response(404)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            error_response = {
                "error": "Not Found",
                "message": f"Path {self.path} not found"
            }
            self.wfile.write(json.dumps(error_response).encode())

    def do_POST(self):
        if self.path == '/api/echo':
            content_length = int(self.headers['Content-Length'])
            post_data = self.rfile.read(content_length)
            
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            response = {
                "message": "Echo endpoint",
                "received_data": post_data.decode('utf-8'),
                "status": "success"
            }
            self.wfile.write(json.dumps(response, indent=2).encode())
        else:
            self.send_response(404)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            
            error_response = {
                "error": "Not Found",
                "message": f"POST endpoint {self.path} not found"
            }
            self.wfile.write(json.dumps(error_response).encode())

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
