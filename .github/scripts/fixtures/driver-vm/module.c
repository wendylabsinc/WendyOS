// SPDX-License-Identifier: GPL-2.0-only
// No device binding, I/O, timers or host references. Only a live version value.
#include <linux/init.h>
#include <linux/module.h>
#include <linux/moduleparam.h>
#include <linux/errno.h>
#ifndef FIXTURE_VERSION
#error FIXTURE_VERSION must be supplied by the private fixture build
#endif
static int fixture_version = FIXTURE_VERSION;
module_param_named(version, fixture_version, int, 0444);
static int __init fixture_init(void)
{
#ifdef FIXTURE_FAIL_INIT
    return -EIO;
#else
    return 0;
#endif
}
static void __exit fixture_exit(void) {}
module_init(fixture_init);
module_exit(fixture_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("Disposable Wendy driver lifecycle VM fixture");
