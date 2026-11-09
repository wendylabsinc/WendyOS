import Foundation
import SwiftProtobuf
import Testing
import WendyAgentGRPC

@Suite
struct AgentMoveWireTests {
    // Encoded from WendyOS 78e94b382de14253214381e750dba3ac2417bbf0 descriptors.
    @Test
    func `D1 shared type rename preserves baseline wire`() throws {
        let wire = Data([
            0x08, 0x03, 0x10, 0xf9, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01,
        ])
        let value = try Wendy_Agent_Services_V1_RestartPolicy(serializedBytes: wire)
        #expect(value.mode == .onFailure)
        #expect(value.onFailureMaxRetries == -7)
        #expect(try value.serializedData() == wire)
        #expect(
            Wendy_Agent_Services_V1_RestartPolicy.protoMessageName
                == "wendy.agent.services.v1.RestartPolicy"
        )
    }

    @Test
    func `D2 sensorlink rename preserves baseline wire`() throws {
        let wire = Data([
            0x08, 0x2a, 0x10, 0x2a, 0x18, 0x2a, 0x20, 0x2a, 0x28, 0x2a, 0x32, 0x06, 0x00, 0xff,
            0x77, 0x69, 0x72, 0x65,
        ])
        let value = try Wendylite_V1_SensorData(serializedBytes: wire)
        #expect(value.channelID == 42)
        #expect(value.frameSeq == 42)
        #expect(value.chunkSeq == 42)
        #expect(value.tsUs == 42)
        #expect(value.flags == 42)
        #expect(value.payload == Data([0, 255, 0x77, 0x69, 0x72, 0x65]))
        #expect(try value.serializedData() == wire)
        #expect(Wendylite_V1_SensorData.protoMessageName == "wendylite.v1.SensorData")
    }
}
