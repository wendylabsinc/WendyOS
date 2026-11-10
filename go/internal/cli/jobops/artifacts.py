"""Fixed, bounded artifact snapshot transport; binary data is consumed by CLI."""

import base64, hashlib, json, os, stat, sys
from pathlib import Path


def main():
    request = json.loads(sys.argv[1])
    root = Path(request["path"]).resolve(strict=True)
    files = request["files"]
    known = request.get("known", {})
    if root == Path("/") or not isinstance(files, list) or not 1 <= len(files) <= 64:
        raise ValueError("one to 64 selected files are required")
    out = []
    total = 0
    for name in files:
        if (
            not isinstance(name, str)
            or not name
            or len(name) > 1024
            or Path(name).is_absolute()
            or ".." in Path(name).parts
        ):
            raise ValueError("files must be safe relative paths")
        directory = os.open(
            str(root), os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_NOFOLLOW", 0)
        )
        try:
            parts = Path(name).parts
            for part in parts[:-1]:
                next_directory = os.open(
                    part,
                    os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_NOFOLLOW", 0),
                    dir_fd=directory,
                )
                os.close(directory)
                directory = next_directory
            fd = os.open(
                parts[-1],
                os.O_RDONLY
                | getattr(os, "O_NOFOLLOW", 0)
                | getattr(os, "O_NONBLOCK", 0),
                dir_fd=directory,
            )
        finally:
            os.close(directory)
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_size > 16 * 1024 * 1024:
                raise ValueError("artifact must be a regular file of at most 16 MiB")
            chunks = []
            size = 0
            while size <= before.st_size:
                chunk = os.read(fd, min(65536, before.st_size + 1 - size))
                if not chunk:
                    break
                chunks.append(chunk)
                size += len(chunk)
            data = b"".join(chunks)
            after = os.fstat(fd)
            if (
                len(data) != before.st_size
                or before.st_mtime_ns != after.st_mtime_ns
                or before.st_size != after.st_size
            ):
                raise ValueError("artifact changed during snapshot; retry")
        finally:
            os.close(fd)
        digest = hashlib.sha256(data).hexdigest()
        skip = known.get(name) == digest
        total += 0 if skip else len(data)
        if total > 16 * 1024 * 1024:
            raise ValueError("selected transfer exceeds 16 MiB; select fewer files")
        item = {"path": name, "sha256": digest, "bytes": len(data), "unchanged": skip}
        if not skip:
            item["data"] = base64.b64encode(data).decode()
        out.append(item)
    print(
        "WENDY_JOB "
        + json.dumps({"files": out, "complete": True}, separators=(",", ":")),
        flush=True,
    )


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(
            "WENDY_JOB " + json.dumps({"error": str(exc)[:800]}, separators=(",", ":")),
            flush=True,
        )
        sys.exit(1)
