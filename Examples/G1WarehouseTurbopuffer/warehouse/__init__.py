"""A Unitree G1 organises a warehouse with turbopuffer as its memory."""

import os

# numpy's OpenBLAS starts a worker thread per core, and between the arm IK's tiny matrix
# operations those workers spin, using several cores for nothing. One thread is as fast here.
# (Set before anything imports numpy.)
for _var in ("OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS"):
    os.environ.setdefault(_var, "1")
