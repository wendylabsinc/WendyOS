# Watch a camera with data documents

Three single-purpose documents:

- `front-door.inference.yaml` runs a person detector on every camera and
  emits `front-door` when someone arrives.
- `front-door.watch.yaml` sends each arrival to Wendy Cloud.
- `front-door-clips.collection.yaml` records an episode around each arrival.
  Leave it out to keep nothing.

Apply them with `wendy data apply <file>`, check them with
`wendy data documents`, and remove the inference and watch with
`wendy data remove inference/front-door` and `wendy data remove
watch/front-door`.
