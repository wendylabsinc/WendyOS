from fastapi import FastAPI

app = FastAPI()


@app.get("/hello-world")
async def hello_world():
    return {"message": "Hello from WendyOS!", "status": "ok"}
