extension Machine {
    public func command(_ command: String) -> MachineCommand {
        MachineCommand(machine: self, command: command)
    }
}
