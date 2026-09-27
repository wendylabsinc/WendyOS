import SwiftProtobuf
import Testing
import WendyAgentGRPC

@Test func sensorProtoTypesGenerated() {
    var m = Wendy_Lite_Sensorlink_SensorManifest()
    m.sensors.append(Wendy_Lite_Sensorlink_SensorDescriptor())
    let req = Wendy_Agent_Services_V2_StreamSensorsRequest.with { $0.channelID = [1] }
    #expect(m.sensors.count == 1)
    #expect(req.channelID == [1])
}
