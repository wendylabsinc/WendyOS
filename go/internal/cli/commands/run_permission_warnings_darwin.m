#import <AVFoundation/AVFoundation.h>
#import <CoreBluetooth/CoreBluetooth.h>

static int wendy_map_capture_status(AVAuthorizationStatus status) {
    switch (status) {
        case AVAuthorizationStatusAuthorized:
            return 0;
        case AVAuthorizationStatusDenied:
        case AVAuthorizationStatusNotDetermined:
            return 1;
        case AVAuthorizationStatusRestricted:
        default:
            return 2;
    }
}

static int wendy_map_bluetooth_status(CBManagerAuthorization status) {
    switch (status) {
        case CBManagerAuthorizationAllowedAlways:
            return 0;
        case CBManagerAuthorizationDenied:
        case CBManagerAuthorizationNotDetermined:
            return 1;
        case CBManagerAuthorizationRestricted:
        default:
            return 2;
    }
}

int wendy_camera_permission_status(void) {
    return wendy_map_capture_status([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeVideo]);
}

int wendy_microphone_permission_status(void) {
    return wendy_map_capture_status([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio]);
}

int wendy_bluetooth_permission_status(void) {
    return wendy_map_bluetooth_status([CBCentralManager authorization]);
}
