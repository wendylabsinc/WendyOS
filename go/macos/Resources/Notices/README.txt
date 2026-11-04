This app includes libusb, dynamically linked and unmodified.

libusb is distributed under the GNU Lesser General Public License, version 2.1
or later. The license is included in libusb-COPYING.txt.

Upstream source and releases: https://github.com/libusb/libusb
The version included by a build is the installed pkg-config libusb-1.0 version.
The development build validated here used libusb 1.0.30.

The library is in Contents/Frameworks/libusb-1.0.dylib and can be replaced with
a compatible build. Re-sign the app after replacing its library. No product
restriction is imposed on debugging modifications to this LGPL component.

Recorded ROS 2 playback includes Foxglove's MCAP Go library, version 1.7.4
(MIT, mcap-LICENSE.txt), and go-sqlite3, version 1.14.32
(MIT, go-sqlite3-LICENSE.txt). go-sqlite3 includes SQLite, which is in the
public domain.

https://github.com/foxglove/mcap
https://github.com/mattn/go-sqlite3
https://www.sqlite.org/copyright.html

The embedded installer terminal uses SwiftTerm 1.18.0 (MIT).
See SwiftTerm-LICENSE.txt and https://github.com/migueldeicaza/SwiftTerm.

Offline installation models render with Three.js 0.172.0 (MIT,
three-LICENSE.txt) and Google's Draco decoder (Apache 2.0,
draco-LICENSE.txt). The model provenance and hardware references are in
Install3D/README.md in the app resources.
