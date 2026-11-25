#include <jni.h>
#include <stdlib.h>
#include "_cgo_export.h"

JNIEXPORT jbyteArray JNICALL
Java_dev_wendy_android_NativeBridge_invoke(JNIEnv *env, jobject self, jbyteArray request) {
    (void)self;
    if (request == NULL) return NULL;
    jsize length = (*env)->GetArrayLength(env, request);
    if (length > 2 * 1024 * 1024) {
        jclass cls = (*env)->FindClass(env, "java/lang/IllegalArgumentException");
        (*env)->ThrowNew(env, cls, "Request exceeds 2 MiB");
        return NULL;
    }
    jbyte *input = (*env)->GetByteArrayElements(env, request, NULL);
    if (input == NULL) return NULL;
    int outputLength = 0;
    void *output = WendyInvoke((char *)input, length, &outputLength);
    (*env)->ReleaseByteArrayElements(env, request, input, JNI_ABORT);
    jbyteArray result = (*env)->NewByteArray(env, outputLength);
    if (result != NULL) (*env)->SetByteArrayRegion(env, result, 0, outputLength, output);
    free(output);
    return result;
}

static JavaVM *vm;
static jobject platform;
static jmethodID platformCall;

JNIEXPORT void JNICALL
Java_dev_wendy_android_NativeBridge_attach(JNIEnv *env, jobject self, jobject services) {
    (void)self;
    (*env)->GetJavaVM(env, &vm);
    if (platform != NULL) return;
    platform = (*env)->NewGlobalRef(env, services);
    jclass cls = (*env)->GetObjectClass(env, services);
    platformCall = (*env)->GetMethodID(env, cls, "invoke", "([B)[B");
    (*env)->DeleteLocalRef(env, cls);
}

void *WendyPlatform(void *input, int length, int *outputLength) {
    *outputLength = 0;
    if (vm == NULL || platform == NULL || platformCall == NULL) return NULL;
    JNIEnv *env = NULL;
    int attached = (*vm)->GetEnv(vm, (void **)&env, JNI_VERSION_1_6) != JNI_OK;
    if (attached && (*vm)->AttachCurrentThread(vm, &env, NULL) != JNI_OK) return NULL;
    void *output = NULL;
    jbyteArray request = (*env)->NewByteArray(env, length);
    if (request != NULL) {
        (*env)->SetByteArrayRegion(env, request, 0, length, input);
        jbyteArray result = (*env)->CallObjectMethod(env, platform, platformCall, request);
        if ((*env)->ExceptionCheck(env)) { (*env)->ExceptionClear(env); }
        else if (result != NULL) {
            jsize size = (*env)->GetArrayLength(env, result);
            output = malloc(size);
            if (output != NULL) { (*env)->GetByteArrayRegion(env, result, 0, size, output); *outputLength = size; }
            (*env)->DeleteLocalRef(env, result);
        }
        (*env)->DeleteLocalRef(env, request);
    }
    if (attached) (*vm)->DetachCurrentThread(vm);
    return output;
}
