// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package ice

import (
	"net"
	"strings"
)

func validateIPString(ipStr string) (net.IP, bool, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, false, ErrInvalidNAT1To1IPMapping
	}

	return ip, (ip.To4() != nil), nil
}

// ipMapping holds the mapping of local and external IP address
//
//	for a particular IP family.
type ipMapping struct {
	ipSole net.IP            // When non-nil, this is the sole external IP for one local IP assumed
	ipMap  map[string]net.IP // Local-to-external IP mapping (k: local, v: external)
	valid  bool              // If not set any external IP, valid is false
}

func (m *ipMapping) setSoleIP(ip net.IP) error {
	if m.ipSole != nil || len(m.ipMap) > 0 {
		return ErrInvalidNAT1To1IPMapping
	}

	m.ipSole = ip
	m.valid = true

	return nil
}

func (m *ipMapping) addIPMapping(locIP, extIP net.IP) error {
	if m.ipSole != nil {
		return ErrInvalidNAT1To1IPMapping
	}

	locIPStr := locIP.String()

	// Check if dup of local IP
	if _, ok := m.ipMap[locIPStr]; ok {
		return ErrInvalidNAT1To1IPMapping
	}

	m.ipMap[locIPStr] = extIP
	m.valid = true

	return nil
}

func (m *ipMapping) findExternalIP(locIP net.IP) (net.IP, error) {
	if !m.valid {
		return locIP, nil
	}

	if m.ipSole != nil {
		return m.ipSole, nil
	}

	extIP, ok := m.ipMap[locIP.String()]
	if !ok {
		return nil, ErrExternalMappedIPNotFound
	}

	return extIP, nil
}

type externalIPMapper struct {
	ipv4Mapping   ipMapping
	ipv6Mapping   ipMapping
	candidateType CandidateType
}

//nolint:gocognit,cyclop
func newExternalIPMapper(
	candidateType CandidateType,
	ips []string,
) (*externalIPMapper, error) {
	if len(ips) == 0 {
		return nil, nil //nolint:nilnil
	}
	if candidateType == CandidateTypeUnspecified {
		candidateType = CandidateTypeHost // Defaults to host
	} else if candidateType != CandidateTypeHost && candidateType != CandidateTypeServerReflexive {
		return nil, ErrUnsupportedNAT1To1IPCandidateType
	}

	mapper := &externalIPMapper{
		ipv4Mapping:   ipMapping{ipMap: map[string]net.IP{}},
		ipv6Mapping:   ipMapping{ipMap: map[string]net.IP{}},
		candidateType: candidateType,
	}

	for _, extIPStr := range ips {
		ipPair := strings.Split(extIPStr, "/")
		if len(ipPair) == 0 || len(ipPair) > 2 {
			return nil, ErrInvalidNAT1To1IPMapping
		}

		extIP, isExtIPv4, err := validateIPString(ipPair[0])
		if err != nil {
			return nil, err
		}
		if len(ipPair) == 1 { //nolint:nestif
			if isExtIPv4 {
				if err := mapper.ipv4Mapping.setSoleIP(extIP); err != nil {
					return nil, err
				}
			} else {
				if err := mapper.ipv6Mapping.setSoleIP(extIP); err != nil {
					return nil, err
				}
			}
		} else {
			locIP, isLocIPv4, err := validateIPString(ipPair[1])
			if err != nil {
				return nil, err
			}
			if isExtIPv4 {
				if !isLocIPv4 {
					return nil, ErrInvalidNAT1To1IPMapping
				}

				if err := mapper.ipv4Mapping.addIPMapping(locIP, extIP); err != nil {
					return nil, err
				}
			} else {
				if isLocIPv4 {
					return nil, ErrInvalidNAT1To1IPMapping
				}

				if err := mapper.ipv6Mapping.addIPMapping(locIP, extIP); err != nil {
					return nil, err
				}
			}
		}
	}

	return mapper, nil
}

func (m *externalIPMapper) findExternalIP(localIPStr string) (net.IP, error) {
	locIP, isLocIPv4, err := validateIPString(localIPStr)
	if err != nil {
		return nil, err
	}

	if isLocIPv4 {
		return m.ipv4Mapping.findExternalIP(locIP)
	}

	return m.ipv6Mapping.findExternalIP(locIP)
}
