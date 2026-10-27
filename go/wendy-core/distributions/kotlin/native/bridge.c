#include <jni.h>
#include <stdint.h>
#include "native_loader.h"

JNIEXPORT jlong JNICALL Java_dev_wendy_core_NativeBridge_open(JNIEnv *env, jobject self, jbyteArray path) {
    (void)self;
    jsize const length = (*env)->GetArrayLength(env, path);
    char *bytes = (char *)malloc((size_t)length + 1);
    if (bytes == NULL) {
        jclass error = (*env)->FindClass(env, "java/lang/OutOfMemoryError");
        if (error != NULL) (*env)->ThrowNew(env, error, "Cannot allocate native path");
        return 0;
    }
    (*env)->GetByteArrayRegion(env, path, 0, length, (jbyte *)bytes);
    if ((*env)->ExceptionCheck(env)) { free(bytes); return 0; }
    bytes[length] = '\0';
    WendyCoreLibrary *library = WendyCoreLibraryOpen(bytes);
    free(bytes);
    if (library == NULL) {
        jclass error = (*env)->FindClass(env, "java/lang/UnsatisfiedLinkError");
        if (error != NULL) (*env)->ThrowNew(env, error, "Cannot load Wendy Core native library");
        return 0;
    }
    return (jlong)(intptr_t)library;
}

JNIEXPORT jint JNICALL Java_dev_wendy_core_NativeBridge_hello(JNIEnv *env, jobject self, jlong handle) {
    (void)env; (void)self;
    WendyCoreLibrary const *library = (WendyCoreLibrary const *)(intptr_t)handle;
    return library->hello();
}

JNIEXPORT jstring JNICALL Java_dev_wendy_core_NativeBridge_version(JNIEnv *env, jobject self, jlong handle) {
    (void)self;
    WendyCoreLibrary const *library = (WendyCoreLibrary const *)(intptr_t)handle;
    return (*env)->NewStringUTF(env, library->version());
}
