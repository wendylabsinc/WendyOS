// DO NOT EDIT.
// swift-format-ignore-file
// swiftlint:disable all
//
// Hand-written Swift protobuf bindings.
// Source: wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto

import Foundation
import SwiftProtobuf

// MARK: - FileSyncEntry

public struct Wendy_Agent_Services_V1_FileSyncEntry: Sendable {
  public var path: String = String()
  public var size: Int64 = 0
  public var sha256: String = String()
  public var mode: UInt32 = 0
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncRequest

public struct Wendy_Agent_Services_V1_FileSyncRequest: Sendable {
  public var requestType: Wendy_Agent_Services_V1_FileSyncRequest.OneOf_RequestType? = nil

  public var start: Wendy_Agent_Services_V1_FileSyncStart {
    get {
      if case .start(let v)? = requestType { return v }
      return Wendy_Agent_Services_V1_FileSyncStart()
    }
    set { requestType = .start(newValue) }
  }

  public var chunk: Wendy_Agent_Services_V1_FileSyncChunk {
    get {
      if case .chunk(let v)? = requestType { return v }
      return Wendy_Agent_Services_V1_FileSyncChunk()
    }
    set { requestType = .chunk(newValue) }
  }

  public var commit: Wendy_Agent_Services_V1_FileSyncCommit {
    get {
      if case .commit(let v)? = requestType { return v }
      return Wendy_Agent_Services_V1_FileSyncCommit()
    }
    set { requestType = .commit(newValue) }
  }

  public var unknownFields = SwiftProtobuf.UnknownStorage()

  public enum OneOf_RequestType: Equatable, Sendable {
    case start(Wendy_Agent_Services_V1_FileSyncStart)
    case chunk(Wendy_Agent_Services_V1_FileSyncChunk)
    case commit(Wendy_Agent_Services_V1_FileSyncCommit)
  }

  public init() {}
}

// MARK: - FileSyncStart

public struct Wendy_Agent_Services_V1_FileSyncStart: Sendable {
  public var appID: String = String()
  public var manifest: [Wendy_Agent_Services_V1_FileSyncEntry] = []
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncChunk

public struct Wendy_Agent_Services_V1_FileSyncChunk: Sendable {
  public var path: String = String()
  public var data: Data = Data()
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncCommit

public struct Wendy_Agent_Services_V1_FileSyncCommit: Sendable {
  public var path: String = String()
  public var sha256: String = String()
  public var size: Int64 = 0
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncResponse

public struct Wendy_Agent_Services_V1_FileSyncResponse: Sendable {
  public var responseType: Wendy_Agent_Services_V1_FileSyncResponse.OneOf_ResponseType? = nil

  public var manifest: Wendy_Agent_Services_V1_FileSyncManifest {
    get {
      if case .manifest(let v)? = responseType { return v }
      return Wendy_Agent_Services_V1_FileSyncManifest()
    }
    set { responseType = .manifest(newValue) }
  }

  public var ack: Wendy_Agent_Services_V1_FileSyncAck {
    get {
      if case .ack(let v)? = responseType { return v }
      return Wendy_Agent_Services_V1_FileSyncAck()
    }
    set { responseType = .ack(newValue) }
  }

  public var complete: Wendy_Agent_Services_V1_FileSyncComplete {
    get {
      if case .complete(let v)? = responseType { return v }
      return Wendy_Agent_Services_V1_FileSyncComplete()
    }
    set { responseType = .complete(newValue) }
  }

  public var unknownFields = SwiftProtobuf.UnknownStorage()

  public enum OneOf_ResponseType: Equatable, Sendable {
    case manifest(Wendy_Agent_Services_V1_FileSyncManifest)
    case ack(Wendy_Agent_Services_V1_FileSyncAck)
    case complete(Wendy_Agent_Services_V1_FileSyncComplete)
  }

  public init() {}
}

// MARK: - FileSyncManifest

public struct Wendy_Agent_Services_V1_FileSyncManifest: Sendable {
  public var files: [Wendy_Agent_Services_V1_FileSyncEntry] = []
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncAck

public struct Wendy_Agent_Services_V1_FileSyncAck: Sendable {
  public var path: String = String()
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - FileSyncComplete

public struct Wendy_Agent_Services_V1_FileSyncComplete: Sendable {
  public var unknownFields = SwiftProtobuf.UnknownStorage()
  public init() {}
}

// MARK: - SwiftProtobuf conformance

fileprivate let _protobuf_package = "wendy.agent.services.v1"

extension Wendy_Agent_Services_V1_FileSyncEntry: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncEntry"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "path"),
    2: .same(proto: "size"),
    3: .same(proto: "sha256"),
    4: .same(proto: "mode"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeSingularStringField(value: &self.path) }()
      case 2: try { try decoder.decodeSingularInt64Field(value: &self.size) }()
      case 3: try { try decoder.decodeSingularStringField(value: &self.sha256) }()
      case 4: try { try decoder.decodeSingularUInt32Field(value: &self.mode) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.path.isEmpty { try visitor.visitSingularStringField(value: self.path, fieldNumber: 1) }
    if self.size != 0 { try visitor.visitSingularInt64Field(value: self.size, fieldNumber: 2) }
    if !self.sha256.isEmpty { try visitor.visitSingularStringField(value: self.sha256, fieldNumber: 3) }
    if self.mode != 0 { try visitor.visitSingularUInt32Field(value: self.mode, fieldNumber: 4) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncEntry, rhs: Wendy_Agent_Services_V1_FileSyncEntry) -> Bool {
    if lhs.path != rhs.path { return false }
    if lhs.size != rhs.size { return false }
    if lhs.sha256 != rhs.sha256 { return false }
    if lhs.mode != rhs.mode { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncRequest: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncRequest"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "start"),
    2: .same(proto: "chunk"),
    3: .same(proto: "commit"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try {
        var v: Wendy_Agent_Services_V1_FileSyncStart?
        var hadOneofValue = false
        if let current = self.requestType { hadOneofValue = true; if case .start(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.requestType = .start(v) }
      }()
      case 2: try {
        var v: Wendy_Agent_Services_V1_FileSyncChunk?
        var hadOneofValue = false
        if let current = self.requestType { hadOneofValue = true; if case .chunk(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.requestType = .chunk(v) }
      }()
      case 3: try {
        var v: Wendy_Agent_Services_V1_FileSyncCommit?
        var hadOneofValue = false
        if let current = self.requestType { hadOneofValue = true; if case .commit(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.requestType = .commit(v) }
      }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    switch self.requestType {
    case .start?: try {
      guard case .start(let v)? = self.requestType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 1)
    }()
    case .chunk?: try {
      guard case .chunk(let v)? = self.requestType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 2)
    }()
    case .commit?: try {
      guard case .commit(let v)? = self.requestType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 3)
    }()
    case nil: break
    }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncRequest, rhs: Wendy_Agent_Services_V1_FileSyncRequest) -> Bool {
    if lhs.requestType != rhs.requestType { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncStart: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncStart"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .standard(proto: "app_id"),
    2: .same(proto: "manifest"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeSingularStringField(value: &self.appID) }()
      case 2: try { try decoder.decodeRepeatedMessageField(value: &self.manifest) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.appID.isEmpty { try visitor.visitSingularStringField(value: self.appID, fieldNumber: 1) }
    if !self.manifest.isEmpty { try visitor.visitRepeatedMessageField(value: self.manifest, fieldNumber: 2) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncStart, rhs: Wendy_Agent_Services_V1_FileSyncStart) -> Bool {
    if lhs.appID != rhs.appID { return false }
    if lhs.manifest != rhs.manifest { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncChunk: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncChunk"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "path"),
    2: .same(proto: "data"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeSingularStringField(value: &self.path) }()
      case 2: try { try decoder.decodeSingularBytesField(value: &self.data) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.path.isEmpty { try visitor.visitSingularStringField(value: self.path, fieldNumber: 1) }
    if !self.data.isEmpty { try visitor.visitSingularBytesField(value: self.data, fieldNumber: 2) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncChunk, rhs: Wendy_Agent_Services_V1_FileSyncChunk) -> Bool {
    if lhs.path != rhs.path { return false }
    if lhs.data != rhs.data { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncCommit: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncCommit"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "path"),
    2: .same(proto: "sha256"),
    3: .same(proto: "size"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeSingularStringField(value: &self.path) }()
      case 2: try { try decoder.decodeSingularStringField(value: &self.sha256) }()
      case 3: try { try decoder.decodeSingularInt64Field(value: &self.size) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.path.isEmpty { try visitor.visitSingularStringField(value: self.path, fieldNumber: 1) }
    if !self.sha256.isEmpty { try visitor.visitSingularStringField(value: self.sha256, fieldNumber: 2) }
    if self.size != 0 { try visitor.visitSingularInt64Field(value: self.size, fieldNumber: 3) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncCommit, rhs: Wendy_Agent_Services_V1_FileSyncCommit) -> Bool {
    if lhs.path != rhs.path { return false }
    if lhs.sha256 != rhs.sha256 { return false }
    if lhs.size != rhs.size { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncResponse: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncResponse"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "manifest"),
    2: .same(proto: "ack"),
    3: .same(proto: "complete"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try {
        var v: Wendy_Agent_Services_V1_FileSyncManifest?
        var hadOneofValue = false
        if let current = self.responseType { hadOneofValue = true; if case .manifest(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.responseType = .manifest(v) }
      }()
      case 2: try {
        var v: Wendy_Agent_Services_V1_FileSyncAck?
        var hadOneofValue = false
        if let current = self.responseType { hadOneofValue = true; if case .ack(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.responseType = .ack(v) }
      }()
      case 3: try {
        var v: Wendy_Agent_Services_V1_FileSyncComplete?
        var hadOneofValue = false
        if let current = self.responseType { hadOneofValue = true; if case .complete(let m) = current { v = m } }
        try decoder.decodeSingularMessageField(value: &v)
        if let v = v { if hadOneofValue { try decoder.handleConflictingOneOf() }; self.responseType = .complete(v) }
      }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    switch self.responseType {
    case .manifest?: try {
      guard case .manifest(let v)? = self.responseType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 1)
    }()
    case .ack?: try {
      guard case .ack(let v)? = self.responseType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 2)
    }()
    case .complete?: try {
      guard case .complete(let v)? = self.responseType else { preconditionFailure() }
      try visitor.visitSingularMessageField(value: v, fieldNumber: 3)
    }()
    case nil: break
    }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncResponse, rhs: Wendy_Agent_Services_V1_FileSyncResponse) -> Bool {
    if lhs.responseType != rhs.responseType { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncManifest: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncManifest"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "files"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeRepeatedMessageField(value: &self.files) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.files.isEmpty { try visitor.visitRepeatedMessageField(value: self.files, fieldNumber: 1) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncManifest, rhs: Wendy_Agent_Services_V1_FileSyncManifest) -> Bool {
    if lhs.files != rhs.files { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncAck: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncAck"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [
    1: .same(proto: "path"),
  ]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let fieldNumber = try decoder.nextFieldNumber() {
      switch fieldNumber {
      case 1: try { try decoder.decodeSingularStringField(value: &self.path) }()
      default: break
      }
    }
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    if !self.path.isEmpty { try visitor.visitSingularStringField(value: self.path, fieldNumber: 1) }
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncAck, rhs: Wendy_Agent_Services_V1_FileSyncAck) -> Bool {
    if lhs.path != rhs.path { return false }
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}

extension Wendy_Agent_Services_V1_FileSyncComplete: SwiftProtobuf.Message, SwiftProtobuf._MessageImplementationBase, SwiftProtobuf._ProtoNameProviding {
  public static let protoMessageName: String = _protobuf_package + ".FileSyncComplete"
  public static let _protobuf_nameMap: SwiftProtobuf._NameMap = [:]

  public mutating func decodeMessage<D: SwiftProtobuf.Decoder>(decoder: inout D) throws {
    while let _ = try decoder.nextFieldNumber() {}
  }

  public func traverse<V: SwiftProtobuf.Visitor>(visitor: inout V) throws {
    try unknownFields.traverse(visitor: &visitor)
  }

  public static func == (lhs: Wendy_Agent_Services_V1_FileSyncComplete, rhs: Wendy_Agent_Services_V1_FileSyncComplete) -> Bool {
    if lhs.unknownFields != rhs.unknownFields { return false }
    return true
  }
}
