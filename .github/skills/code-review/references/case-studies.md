# Evidence from WendyOS reviews

Source period: August 4 through November 4, 2026. These are historical decisions and
merge-time observations. Verify current code and design before applying them. A merge
does not make earlier objections wrong; a closure does not mean the work never shipped.

## Architectural objections that changed the implementation

| Case | What mattered | Lesson |
| --- | --- | --- |
| [Thermal metadata, #2434](https://github.com/wendylabsinc/WendyOS/pull/2434) | A thermal-only video field bypassed general sensor recording and rules. | Replaced with Wendy Data streams in #2520. Check downstream consumers and extensibility. |
| [Model watch, #2063](https://github.com/wendylabsinc/WendyOS/pull/2063) and [leases, #2206](https://github.com/wendylabsinc/WendyOS/pull/2206) | Execution backends became entangled with overlapping public control planes. | Preserve useful execution work while reconciling control-plane ownership. |
| [Data rebuild, #1973](https://github.com/wendylabsinc/WendyOS/pull/1973) | Parallel development produced a second device PKI after Cloud v2 supplied one. | Consume the shared enrolled identity; revisit assumptions when dependencies evolve. |
| [RGB-D capture, #2041](https://github.com/wendylabsinc/WendyOS/pull/2041) | A helper exclusively claimed RGB despite a shared-camera promise. | #2254 reused the contract with native capture and shared ownership. Good API shape does not prove resource correctness. |
| [MCP gateway, #2182](https://github.com/wendylabsinc/WendyOS/pull/2182) | Machine authority and missing DPoP proof failed to preserve user authority. | The team later chose a credential-less CLI relay. Trace every hop. |
| [Wi-Fi rollback, #2214](https://github.com/wendylabsinc/WendyOS/pull/2214) | Undoing failed association undid deliberate password preconfiguration. | A narrower CLI change preserved the legitimate workflow. |

## Formal change requests followed by meaningful fixes

| Case | Issue | Correction |
| --- | --- | --- |
| [Lite UUID, #2429](https://github.com/wendylabsinc/WendyOS/pull/2429) | Enrollment changed identity but pins and unenroll still assumed the old one. Tests missed asset equality. | UUID pins, safe lookup, collision handling and equality assertions. |
| [T234 flashing, #2057](https://github.com/wendylabsinc/WendyOS/pull/2057) | Reliability changes rejected existing flashpacks. | Preserve schema 2 and negotiate newer USB behavior. |
| [ONVIF auth, #1974](https://github.com/wendylabsinc/WendyOS/pull/1974) | Failed randomness could produce a replayable token. | Refuse token creation when randomness fails. |
| [Raw video, #1817](https://github.com/wendylabsinc/WendyOS/pull/1817) | A boolean could not express codecs; true would decode as enum value 1 after conversion. | Use an enum and account for experimental old clients. |
| [Camera controls, #1818](https://github.com/wendylabsinc/WendyOS/pull/1818) | Arguments were undiscoverable; messages mixed reads and writes; reset needed persistence semantics. | Enumerate actual controls and separate read, write and reset. |

## Automated objections that were wrong or disputed

- [#1939, string append](https://github.com/wendylabsinc/WendyOS/pull/1939#discussion_r3977527221):
  the Go compilation claim was false. A minimal compilation confirmed the supported
  string-to-byte-slice append during the audit.
- [#1999, CopyFS](https://github.com/wendylabsinc/WendyOS/pull/1999#discussion_r4049971992):
  two objections confused existing files with an existing destination directory.
  Local API documentation and a reproduction confirmed the directory behavior.
- [#2092, video URL](https://github.com/wendylabsinc/WendyOS/pull/2092#discussion_r4117353473):
  MDX preparation already inserted the base path. Historical source and the author's
  preview evidence contradicted the finding.
- [#1984, Python arrays](https://github.com/wendylabsinc/WendyOS/pull/1984#discussion_r4009093845):
  construction from bytes groups them into words. Other findings resolved the wrong
  ROS module or misunderstood capability removal across exec. The audit reproduced
  the array behavior, not the entire hardware/container campaign.
- [#2342, issuer preservation](https://github.com/wendylabsinc/WendyOS/pull/2342#discussion_r4308984690):
  the author explained why USB provisioning installs the P-256 CA twin. Keeping an
  ML-DSA issuer would retain the firmware incompatibility. This is an author-supported
  disposition, not independent proof of every PKI path.

## A merge or passing test did not establish correctness

- [#1909, GPU detection](https://github.com/wendylabsinc/WendyOS/pull/1909#issuecomment-5635338331):
  Qualcomm support addressed the formal request. A later pre-merge review identified
  CUDA false positives and QNN/NPU data under GPU capabilities. Historical merged
  source retained that questioned behavior. The later fix
  [#2044](https://github.com/wendylabsinc/WendyOS/pull/2044) removed library-only CUDA
  detection and put QNN under NPU capabilities. This concern was addressed later;
  do not report it as still open without checking current code.
- [#1537, docs-review CI](https://github.com/wendylabsinc/WendyOS/commit/2a81c3a6):
  checking blockers before truncation fixed one bug. Checking before validation still
  allowed CI failure without a rendered actionable finding.
- [#2035, G1 measurement](https://github.com/wendylabsinc/WendyOS/pull/2035):
  short samples began after movement ended. A longer capture showed substantial
  travel with mode=1, disproving the proposed safety refusal.
- [#1725, test helpers](https://github.com/wendylabsinc/WendyOS/pull/1725):
  the committed tests referenced newMCPServer, whose implementation existed only in
  unstaged work. Validate the review revision, not a developer's combined tree.

## Do not learn the wrong lesson from closure

[#1570](https://github.com/wendylabsinc/WendyOS/pull/1570) was tested and approved, then
replaced by a rebased PR after overlapping discovery work. The audit also proved that
eight PRs marked closed without merge had their final commits in main. Many closures
had no explanation. Use recorded outcomes, successors and ancestry; leave undocumented
intent unknown.
