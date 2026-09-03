/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * NfType - TS 29.510 clause 6.1.6.3.3, Table 6.1.6.3.3-1 "Enumeration NFType".
 *
 * WHY THIS FILE IS HAND-WRITTEN
 * The OpenAPI generator collapsed this schema (a string enum expressed as an
 * anyOf) into an EMPTY Go struct:
 *
 *     type NfType struct {}
 *
 * NfLoadLevelInformation.nfType is a REQUIRED property, so every NF_LOAD
 * response serialised it as the literal `"nfType": {}` - a required field
 * present but carrying no value, which no consumer can use. TS 23.288
 * Table 6.5.3-1 lists "NF type - Type of the NF instance" as an output of NF
 * load analytics, so the value is not optional decoration.
 *
 * CLASSIFICATION: OAI implementation gap (code-generation artefact). The type is
 * restored to what the specification defines - a string from the TS 29.510
 * enumeration - which is a faithful representation, not an extension.
 *
 * Only the enumeration values are declared; the schema's second anyOf branch
 * ("provides forward-compatibility ... not used to encode content defined in
 * the present version of this API") is naturally covered by the string type.
 */

package events

type NfType string

// List of NfType (TS 29.510 Table 6.1.6.3.3-1)
const (
	NFTYPE_NRF       NfType = "NRF"
	NFTYPE_UDM       NfType = "UDM"
	NFTYPE_AMF       NfType = "AMF"
	NFTYPE_SMF       NfType = "SMF"
	NFTYPE_AUSF      NfType = "AUSF"
	NFTYPE_NEF       NfType = "NEF"
	NFTYPE_PCF       NfType = "PCF"
	NFTYPE_SMSF      NfType = "SMSF"
	NFTYPE_NSSF      NfType = "NSSF"
	NFTYPE_UDR       NfType = "UDR"
	NFTYPE_LMF       NfType = "LMF"
	NFTYPE_GMLC      NfType = "GMLC"
	NFTYPE_5G_EIR    NfType = "5G_EIR"
	NFTYPE_SEPP      NfType = "SEPP"
	NFTYPE_UPF       NfType = "UPF"
	NFTYPE_N3IWF     NfType = "N3IWF"
	NFTYPE_AF        NfType = "AF"
	NFTYPE_UDSF      NfType = "UDSF"
	NFTYPE_BSF       NfType = "BSF"
	NFTYPE_CHF       NfType = "CHF"
	NFTYPE_NWDAF     NfType = "NWDAF"
	NFTYPE_PCSCF     NfType = "PCSCF"
	NFTYPE_CBCF      NfType = "CBCF"
	NFTYPE_HSS       NfType = "HSS"
	NFTYPE_UCMF      NfType = "UCMF"
	NFTYPE_SOR_AF    NfType = "SOR_AF"
	NFTYPE_SPAF      NfType = "SPAF"
	NFTYPE_MME       NfType = "MME"
	NFTYPE_SCSAS     NfType = "SCSAS"
	NFTYPE_SCEF      NfType = "SCEF"
	NFTYPE_SCP       NfType = "SCP"
	NFTYPE_NSSAAF    NfType = "NSSAAF"
	NFTYPE_ICSCF     NfType = "ICSCF"
	NFTYPE_SCSCF     NfType = "SCSCF"
	NFTYPE_DRA       NfType = "DRA"
	NFTYPE_IMS_AS    NfType = "IMS_AS"
	NFTYPE_AANF      NfType = "AANF"
	NFTYPE_5G_DDNMF  NfType = "5G_DDNMF"
	NFTYPE_NSACF     NfType = "NSACF"
	NFTYPE_MFAF      NfType = "MFAF"
	NFTYPE_EASDF     NfType = "EASDF"
	NFTYPE_DCCF      NfType = "DCCF"
	NFTYPE_MB_SMF    NfType = "MB_SMF"
	NFTYPE_TSCTSF    NfType = "TSCTSF"
	NFTYPE_ADRF      NfType = "ADRF"
	NFTYPE_GBA_BSF   NfType = "GBA_BSF"
	NFTYPE_CEF       NfType = "CEF"
	NFTYPE_MB_UPF    NfType = "MB_UPF"
	NFTYPE_NSWOF     NfType = "NSWOF"
	NFTYPE_PKMF      NfType = "PKMF"
	NFTYPE_MNPF      NfType = "MNPF"
	NFTYPE_SMS_GMSC  NfType = "SMS_GMSC"
	NFTYPE_SMS_IWMSC NfType = "SMS_IWMSC"
	NFTYPE_MBSF      NfType = "MBSF"
	NFTYPE_MBSTF     NfType = "MBSTF"
	NFTYPE_PANF      NfType = "PANF"
	NFTYPE_TNGF      NfType = "TNGF"
	NFTYPE_W_AGF     NfType = "W_AGF"
	NFTYPE_TWIF      NfType = "TWIF"
	NFTYPE_NSSAA_AF  NfType = "NSSAA_AF"
)

// AssertNfTypeRequired checks if the required fields are not zero-ed
func AssertNfTypeRequired(obj NfType) error {
	return nil
}

// AssertRecurseNfTypeRequired recursively checks if required fields are not zero-ed in a nested slice.
// Accepts only nested slice of NfType (e.g. [][]NfType), otherwise ErrTypeAssertionError is thrown.
func AssertRecurseNfTypeRequired(objSlice interface{}) error {
	return AssertRecurseInterfaceRequired(objSlice, func(obj interface{}) error {
		aNfType, ok := obj.(NfType)
		if !ok {
			return ErrTypeAssertionError
		}
		return AssertNfTypeRequired(aNfType)
	})
}
