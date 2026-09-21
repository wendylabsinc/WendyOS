"""Draw the recorded MuJoCo trajectories and record the tested source hashes."""
import hashlib
import json
from pathlib import Path
import xml.etree.ElementTree as ET


def main():
    root = Path(__file__).resolve().parent
    results = root / "validation"
    names = ("straight-1.0-left", "corner-1.4-left", "junction-1.4-left", "junction-1.4-right")
    svg = ET.Element("svg", xmlns="http://www.w3.org/2000/svg", width="960", height="720", viewBox="0 0 960 720")
    ET.SubElement(svg, "rect", width="960", height="720", fill="#f7f9fb")
    for index, name in enumerate(names):
        data = json.loads((results / (name + ".json")).read_text())
        x0, y0 = 30 + (index % 2) * 480, 45 + (index // 2) * 350
        title = ET.SubElement(svg, "text", x=str(x0), y=str(y0-12), fill="#182235", **{"font-family":"sans-serif", "font-size":"17"})
        title.text = name.replace("-", " ")
        trace = data["trace"]
        def point(x, y):
            return x0+65+(x+1)*28, y0+145-y*28
        for x in range(-1, 8):
            a,b = point(x,-4),point(x,5)
            ET.SubElement(svg,"line",x1=str(a[0]),y1=str(a[1]),x2=str(b[0]),y2=str(b[1]),stroke="#dfe5ed")
        for y in range(-4,6):
            a,b = point(-1,y),point(7,y)
            ET.SubElement(svg,"line",x1=str(a[0]),y1=str(a[1]),x2=str(b[0]),y2=str(b[1]),stroke="#dfe5ed")
        coords = " ".join(f"{a:.2f},{b:.2f}" for a,b in (point(row["x"],row["y"]) for row in trace))
        ET.SubElement(svg,"polyline",points=coords,fill="none",stroke="#176fc1",**{"stroke-width":"3"})
        for row, color in ((trace[0],"#249160"),(trace[-1],"#c24935")):
            x,y = point(row["x"],row["y"])
            ET.SubElement(svg,"circle",cx=str(x),cy=str(y),r="5",fill=color)
        label = ET.SubElement(svg,"text",x=str(x0),y=str(y0+290),fill="#344154",**{"font-family":"sans-serif","font-size":"13"})
        label.text = f"{data['status']['distance_m']:.2f} m travelled; {data['wall_contacts']} wall contacts. Grid: 1 m."
    ET.ElementTree(svg).write(results / "paths.svg", encoding="unicode")
    hashes = {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
              for path in sorted(root.glob("*.py"))}
    hashes["Dockerfile"] = hashlib.sha256((root / "Dockerfile").read_bytes()).hexdigest()
    manifest = {"source_sha256": hashes, "controller_and_admission_tests": 26, "physics_tests": 7,
                "forward_speed_m_s": .55, "hardware_motion_tested": False,
                "ros": json.loads((results / "ros-corner.json").read_text())}
    (results / "summary.json").write_text(json.dumps(manifest, indent=2, allow_nan=False)+"\n")


if __name__ == "__main__":
    main()
