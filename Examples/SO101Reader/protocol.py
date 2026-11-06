"""Bounded Feetech protocol-0 READs. No motor SDK or register-write API."""

READS = {(3, 2), (56, 8)}
MODELS = {777: "sts3215", 2825: "sts3250", 11272: "sm8512bl"}


class ReaderError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code

    def json(self):
        return {"code": self.code, "message": str(self)}


def request(motor_id, address, count):
    if (type(motor_id) is not int or not 1 <= motor_id <= 253
            or type(address) is not int or type(count) is not int or (address, count) not in READS):
        raise ReaderError("invalid_request", "Only model and present-state READs are allowed")
    packet = bytes((255, 255, motor_id, 4, 2, address, count))
    return packet + bytes((~sum(packet[2:]) & 255,))


def response(frame, motor_id, count):
    if (len(frame) != count + 6 or frame[:2] != b"\xff\xff"
            or frame[2] != motor_id or frame[3] != count + 2):
        raise ReaderError("protocol", "Unexpected response header, motor ID or length")
    if sum(frame[2:]) & 255 != 255:
        raise ReaderError("checksum", "Invalid response checksum")
    return frame[5:-1], frame[4]


def position(word):
    """STS/SMS sign-magnitude bit 15; retain the unsigned word separately."""
    return -(word & 0x7fff) if word & 0x8000 else word
