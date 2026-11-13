#include <stddef.h>
#include <stdio.h>
#include <linux/videodev2.h>

/* These constants also permit an independent cross-target compiler inspection
 * without executing a Linux binary or accessing a V4L2 device. */
const unsigned long format_size = sizeof(struct v4l2_format);
const unsigned long format_union_offset = offsetof(struct v4l2_format, fmt);
const unsigned long buffer_size = sizeof(struct v4l2_buffer);
const unsigned long buffer_memory_offset = offsetof(struct v4l2_buffer, m);
const unsigned long buffer_length_offset = offsetof(struct v4l2_buffer, length);
const unsigned long buffer_request_offset = offsetof(struct v4l2_buffer, request_fd);

int main(void) {
    printf("{\"format_size\":%lu,\"format_union_offset\":%lu,\"buffer_size\":%lu,"
           "\"buffer_memory_offset\":%lu,\"buffer_length_offset\":%lu,\"buffer_request_offset\":%lu,"
           "\"g_fmt\":%lu,\"querybuf\":%lu,\"dqbuf\":%lu}\n",
           format_size, format_union_offset, buffer_size, buffer_memory_offset,
           buffer_length_offset, buffer_request_offset,
           (unsigned long)VIDIOC_G_FMT, (unsigned long)VIDIOC_QUERYBUF, (unsigned long)VIDIOC_DQBUF);
    return 0;
}
