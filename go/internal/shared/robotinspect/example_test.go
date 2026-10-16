package robotinspect_test

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// The document is the product and the text report is one view of it. This is the shape a
// consumer diffs: every observation carries the axis it was taken on and, when measured,
// the window it was sampled over, and the property carries the verdict the core reached.
func ExampleDocument_CanonicalJSON() {
	declared := robotinspect.MustQuantity(75, robotinspect.Degrees,
		robotinspect.WithAxis(robotinspect.AxisHorizontal))
	fromDatasheet, err := robotinspect.NewObservation(declared, robotinspect.Declared,
		robotinspect.Source{Probe: "camerainfo", Origin: "datasheet"})
	if err != nil {
		panic(err)
	}

	measured := robotinspect.MustQuantity(43.4, robotinspect.Degrees,
		robotinspect.WithAxis(robotinspect.AxisVertical))
	fromRobot, err := robotinspect.NewObservation(measured, robotinspect.Measured,
		robotinspect.Source{Probe: "checkerboard", Origin: "topic:/camera/color/camera_info"},
		robotinspect.WithSampling(time.Minute, 27),
		robotinspect.WithConditions(map[string]string{"resolution": "640x480"}))
	if err != nil {
		panic(err)
	}

	doc := robotinspect.Document{
		Schema:      robotinspect.Schema,
		Device:      "unitree-g1-nx-2",
		VendorKind:  "unitree-g1",
		PassiveOnly: true,
		ProbesRun:   []string{"camerainfo", "checkerboard"},
		Properties: []robotinspect.Property{{
			ID:           "camera.color.fov",
			Observations: []robotinspect.Observation{fromDatasheet, fromRobot},
		}},
	}

	out, err := doc.CanonicalJSON()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))

	// Output:
	// {
	//   "schema": "wendy.robot.inspection.v1",
	//   "device": "unitree-g1-nx-2",
	//   "vendorKind": "unitree-g1",
	//   "passiveOnly": true,
	//   "probesRun": [
	//     "camerainfo",
	//     "checkerboard"
	//   ],
	//   "summary": {
	//     "agree": 0,
	//     "disagree": 0,
	//     "incomparable": 1,
	//     "single": 0,
	//     "unknown": 0,
	//     "findings": 1
	//   },
	//   "properties": [
	//     {
	//       "id": "camera.color.fov",
	//       "verdict": "incomparable",
	//       "detail": "not the same measurement: declared is horizontal, measured is vertical",
	//       "observations": [
	//         {
	//           "kind": "declared",
	//           "value": 75,
	//           "unit": "deg",
	//           "axis": "horizontal",
	//           "source": {
	//             "probe": "camerainfo",
	//             "origin": "datasheet"
	//           }
	//         },
	//         {
	//           "kind": "measured",
	//           "value": 43.4,
	//           "unit": "deg",
	//           "axis": "vertical",
	//           "source": {
	//             "probe": "checkerboard",
	//             "origin": "topic:/camera/color/camera_info"
	//           },
	//           "conditions": {
	//             "resolution": "640x480"
	//           },
	//           "sampling": {
	//             "windowMs": 60000,
	//             "samples": 27
	//           }
	//         }
	//       ]
	//     }
	//   ]
	// }
}
