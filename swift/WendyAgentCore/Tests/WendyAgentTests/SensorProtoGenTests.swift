import SwiftProtobuf
import Testing
import WendyAgentGRPC

@Test func sensorProtoTypesGenerated() {
    var m = Wendylite_V1_SensorManifest()
    m.sensors.append(Wendylite_V1_SensorDescriptor())
    let req = Wendy_Agent_Services_V2_StreamSensorsRequest.with { $0.channelID = [1] }
    #expect(m.sensors.count == 1)
    #expect(req.channelID == [1])
}
