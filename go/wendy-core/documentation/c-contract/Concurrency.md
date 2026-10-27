# Concurrency and callbacks

Public objects are safe to retain and release concurrently. Individual operations state any stronger restrictions.

An asynchronous callback:

1. is never invoked before its initiating function returns;
2. is invoked exactly once for a unary operation;
3. may run on a Wendy-owned worker thread;
4. may cancel or retain other Wendy objects; and
5. must not block indefinitely.

Language bindings move completion onto their native abstraction: Swift continuations, Kotlin coroutines, Python awaitables, or another runtime-specific scheduler. That adaptation does not change operation semantics.
