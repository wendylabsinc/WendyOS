# Ownership

Wendy objects are opaque, reference-counted C objects.

- Functions whose names contain `Create` return one owned reference.
- `Retain` adds one reference and returns the same object.
- `Release` removes one reference and accepts `NULL`.
- `Get` returns a borrowed value unless the operation documentation says otherwise.
- Callback-delivered objects are returns and are therefore not const-qualified. The callback documentation states whether ownership transfers.
- Input and local object references use east-const style, such as `WendyDevice const *device`. Return types are not const-qualified.
- Releasing an operation or stream reference does not implicitly cancel it.

Objects allocated by Wendy are released only through Wendy. No Go pointer crosses the ABI.

String operations may return a borrowed, read-only, NUL-terminated UTF-8 string with process lifetime when explicitly documented. As with object returns, the C return type is not const-qualified; callers must neither modify nor free borrowed strings.
