//go:build linux && amd64 && cgo

// AMD uncore (DF P‑state) support via E‑SMI (HSMP).
//
// This file contains only the AMD uncore helpers that call into the E‑SMI C library.
// E‑SMI is x86‑only, so these helpers can only be compiled and resolved on linux/amd64
// with cgo enabled.
// When building for linux/amd64 with cgo, this file will be selected; otherwise the
// empty implementation in `uncore_amd_stub.go` will be selected.

// This structure keeps existing Intel uncore unit tests compatible with both
// macOS and Linux development environments.
package power

/*
#cgo linux,amd64 CFLAGS: -I${SRCDIR}/../../../e-sms/e_smi/include -I${SRCDIR}/../../../e-sms/amd_hsmp -I${SRCDIR}/../../../../../../e-sms/e_smi/include -I${SRCDIR}/../../../../../../e-sms/amd_hsmp
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../../e-sms/e_smi/lib -L${SRCDIR}/../../../../../../e-sms/e_smi/lib -le_smi64
#include <stdint.h>
#include "e_smi/e_smi.h"
*/
import "C"

import (
	"fmt"
)

const (
	amdHsmpKmodName = "amd_hsmp"
)

// writeAMD applies a DF P‑state/range via E‑SMI.
func (u *uncoreFreq) writeAMD(pkgId uint) error {
	if u.min == u.max {
		esmi_status := C.esmi_apb_disable(C.uint32_t(pkgId), C.uint8_t(u.min))
		if esmi_status != 0 {
			return fmt.Errorf("DF Pstate set failed: %s", C.GoString(C.esmi_get_err_msg(esmi_status)))
		}
	} else {
		esmi_status := C.esmi_df_pstate_range_set(C.uint8_t(pkgId), C.uint8_t(u.min), C.uint8_t(u.max))
		if esmi_status != 0 {
			return fmt.Errorf("DF Pstate range set failed: %s", C.GoString(C.esmi_get_err_msg(esmi_status)))
		}
	}
	return nil
}

// initAMDUncore initializes E‑SMI and sets defaults.
func initAMDUncore() error {
	if !checkKernelModuleLoaded(amdHsmpKmodName) {
		return fmt.Errorf("kernel module %s not loaded", amdHsmpKmodName)
	}

	// Initialize ESMI
	esmi_status := C.esmi_init()
	if esmi_status != 0 {
		errMsg := C.esmi_get_err_msg(esmi_status)
		return fmt.Errorf("AMD ESMI Initialization failed:%s", C.GoString(errMsg))
	}

	/* TBD: add platform probe support instead of hardcoded values */
	defaultUncore.min = 0
	defaultUncore.max = 2

	esmi_status = C.esmi_df_pstate_range_set(C.uint8_t(0), C.uint8_t(defaultUncore.min), C.uint8_t(defaultUncore.max))
	if esmi_status != 0 {
		errMsg := C.esmi_get_err_msg(esmi_status)
		return fmt.Errorf("DF Pstate range set failed: %s", C.GoString(errMsg))
	}
	return nil
}
