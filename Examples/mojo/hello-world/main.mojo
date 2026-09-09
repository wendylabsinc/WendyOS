def main():
    print("Hello from Mojo 1.0 on WendyOS!")
    var values = SIMD[DType.int32, 4](1, 2, 3, 4)
    var result = values * 2 + 1
    print("SIMD result:", result)
