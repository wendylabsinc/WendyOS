from flask import Flask, jsonify

app = Flask(__name__)
MESSAGE = "Hello from Stagefile"


@app.get("/")
def hello():
    return jsonify(message=MESSAGE)


@app.get("/health")
def health():
    return jsonify(status="ok")


if __name__ == "__main__":
    # A teaching server. Use a production WSGI server for a production app.
    app.run(host="0.0.0.0", port=8000)
