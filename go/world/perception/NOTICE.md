# Third-party model notice

MoveNet MultiPose Lightning version 1 is a Google model released under the
Apache License 2.0. The ONNX conversion is published by Xenova at
https://huggingface.co/Xenova/movenet-multipose-lightning with the same license.
The exact conversion revision and artifact digest are retained in `model.json`.

The model weights are unmodified. Wendy's adapter resizes image pixels, runs the
ONNX graph, and converts its two ankle outputs back to original pixel coordinates.
The adapter is private Wendy code, separate from the model and its conversion.

The Apache License is reproduced at `licenses/Apache-2.0.txt`.
Source and model cards are linked in `model.json`; original model notices remain
with those sources. This repository does not redistribute the model binary.
