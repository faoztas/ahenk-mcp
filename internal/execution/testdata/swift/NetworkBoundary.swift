// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

import Darwin
import XCTest

final class NetworkBoundary: XCTestCase {
    func testIPv4TCPAndUDP() {
        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_port = UInt16(9).bigEndian
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                verifyDenied(AF_INET, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
    }

    func testIPv6TCPAndUDP() {
        var address = sockaddr_in6()
        address.sin6_len = UInt8(MemoryLayout<sockaddr_in6>.size)
        address.sin6_family = sa_family_t(AF_INET6)
        address.sin6_port = UInt16(9).bigEndian
        address.sin6_addr = in6addr_loopback
        withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                verifyDenied(AF_INET6, $0, socklen_t(MemoryLayout<sockaddr_in6>.size))
            }
        }
    }

    private func verifyDenied(_ family: Int32, _ address: UnsafePointer<sockaddr>, _ size: socklen_t) {
        let tcp = socket(family, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(tcp, 0)
        if tcp >= 0 {
            let outcome = connect(tcp, address, size)
            let denial = errno
            close(tcp)
            XCTAssertEqual(outcome, -1)
            XCTAssertTrue(denial == EPERM || denial == EACCES)
        }

        let udp = socket(family, SOCK_DGRAM, 0)
        XCTAssertGreaterThanOrEqual(udp, 0)
        if udp >= 0 {
            var byte: UInt8 = 0
            let outcome = sendto(udp, &byte, 1, 0, address, size)
            let denial = errno
            close(udp)
            XCTAssertEqual(outcome, -1)
            XCTAssertTrue(denial == EPERM || denial == EACCES)
        }
    }
}
