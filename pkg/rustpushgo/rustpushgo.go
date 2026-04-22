package rustpushgo

// #include <rustpushgo.h>
// #cgo LDFLAGS: -L${SRCDIR}/../../ -lrustpushgo -ldl -lm -lz
// #cgo darwin LDFLAGS: -framework Security -framework SystemConfiguration -framework CoreFoundation -framework Foundation -framework CoreServices -lresolv
// #cgo linux LDFLAGS: -lpthread -lssl -lcrypto -lresolv
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"
)

type RustBuffer struct {
	capacity C.int32_t
	len      C.int32_t
	data     *C.uint8_t
}

func rustBufferToC(rb RustBuffer) C.RustBuffer {
	return *(*C.RustBuffer)(unsafe.Pointer(&rb))
}

func rustBufferFromC(crb C.RustBuffer) RustBuffer {
	return *(*RustBuffer)(unsafe.Pointer(&crb))
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() int
	Capacity() int
}

func RustBufferFromExternal(b RustBufferI) RustBuffer {
	return RustBuffer{
		capacity: C.int(b.Capacity()),
		len:      C.int(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func (cb RustBuffer) Capacity() int {
	return int(cb.capacity)
}

func (cb RustBuffer) Len() int {
	return int(cb.len)
}

func (cb RustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.data)
}

func (cb RustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.data), C.int(cb.len))
	return bytes.NewReader(b)
}

func (cb RustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_rustpushgo_rustbuffer_free(rustBufferToC(cb), status)
		return false
	})
}

func (cb RustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.data), C.int(cb.len))
}

func stringToRustBuffer(str string) RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) RustBuffer {
	if len(b) == 0 {
		return RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) RustBuffer {
		return rustBufferFromC(C.ffi_rustpushgo_rustbuffer_from_bytes(foreign, status))
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) RustBuffer
}

type FfiConverter[GoType any, FfiType any] interface {
	Lift(value FfiType) GoType
	Lower(value GoType) FfiType
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

type FfiRustBufConverter[GoType any, FfiType any] interface {
	FfiConverter[GoType, FfiType]
	BufReader[GoType]
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[U any](converter BufLifter[error], callback func(*C.RustCallStatus) U) (U, error) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)

	return returnValue, err
}

func checkCallStatus(converter BufLifter[error], status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		return converter.Lift(rustBufferFromC(status.errorBuf))
	case 2:
		// when the rust code sees a panic, it tries to construct a rustbuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(rustBufferFromC(status.errorBuf))))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a rustbuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(rustBufferFromC(status.errorBuf))))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError(nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	(&FfiConverterCallbackInterfaceMessageCallback{}).register()
	(&FfiConverterCallbackInterfaceStatusCallback{}).register()
	(&FfiConverterCallbackInterfaceUpdateUsersCallback{}).register()
	uniffiInitContinuationCallback()
	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 24
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_rustpushgo_uniffi_contract_version(uniffiStatus)
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("rustpushgo: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_connect(uniffiStatus)
		})
		if checksum != 48943 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_connect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_create_config_from_hardware_key(uniffiStatus)
		})
		if checksum != 35117 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_create_config_from_hardware_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_create_config_from_hardware_key_with_device_id(uniffiStatus)
		})
		if checksum != 29425 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_create_config_from_hardware_key_with_device_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_create_local_macos_config(uniffiStatus)
		})
		if checksum != 37134 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_create_local_macos_config: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_create_local_macos_config_with_device_id(uniffiStatus)
		})
		if checksum != 44159 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_create_local_macos_config_with_device_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_ford_key_cache_size(uniffiStatus)
		})
		if checksum != 25806 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_ford_key_cache_size: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_init_logger(uniffiStatus)
		})
		if checksum != 38755 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_init_logger: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_login_start(uniffiStatus)
		})
		if checksum != 53356 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_login_start: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_new_client(uniffiStatus)
		})
		if checksum != 28402 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_new_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_register_ford_key(uniffiStatus)
		})
		if checksum != 34188 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_register_ford_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_func_restore_token_provider(uniffiStatus)
		})
		if checksum != 43442 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_func_restore_token_provider: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_diag_full_count(uniffiStatus)
		})
		if checksum != 27287 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_diag_full_count: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_download_attachment(uniffiStatus)
		})
		if checksum != 39378 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_download_attachment: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_download_attachment_avid(uniffiStatus)
		})
		if checksum != 63062 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_download_attachment_avid: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_download_group_photo(uniffiStatus)
		})
		if checksum != 31376 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_download_group_photo: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_dump_chats_json(uniffiStatus)
		})
		if checksum != 18960 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_dump_chats_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_fetch_recent_messages(uniffiStatus)
		})
		if checksum != 26669 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_fetch_recent_messages: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_query_attachments_fallback(uniffiStatus)
		})
		if checksum != 5504 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_query_attachments_fallback: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_supports_avid_download(uniffiStatus)
		})
		if checksum != 12325 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_supports_avid_download: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_sync_attachments(uniffiStatus)
		})
		if checksum != 29066 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_sync_attachments: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_sync_chats(uniffiStatus)
		})
		if checksum != 48464 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_sync_chats: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_cloud_sync_messages(uniffiStatus)
		})
		if checksum != 61309 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_cloud_sync_messages: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_debug_recoverable_zones(uniffiStatus)
		})
		if checksum != 46761 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_debug_recoverable_zones: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_delete_cloud_chats(uniffiStatus)
		})
		if checksum != 24440 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_delete_cloud_chats: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_delete_cloud_messages(uniffiStatus)
		})
		if checksum != 50268 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_delete_cloud_messages: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_fetch_profile(uniffiStatus)
		})
		if checksum != 36346 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_fetch_profile: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_findmy_friends_import(uniffiStatus)
		})
		if checksum != 28320 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_findmy_friends_import: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_findmy_friends_refresh_json(uniffiStatus)
		})
		if checksum != 27180 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_findmy_friends_refresh_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_findmy_phone_refresh_json(uniffiStatus)
		})
		if checksum != 45424 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_findmy_phone_refresh_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_contacts_url(uniffiStatus)
		})
		if checksum != 50659 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_contacts_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_dsid(uniffiStatus)
		})
		if checksum != 24963 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_dsid: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_facetime_client(uniffiStatus)
		})
		if checksum != 2377 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_facetime_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_findmy_client(uniffiStatus)
		})
		if checksum != 58400 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_findmy_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_handles(uniffiStatus)
		})
		if checksum != 2965 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_handles: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_icloud_auth_headers(uniffiStatus)
		})
		if checksum != 46466 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_icloud_auth_headers: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_passwords_client(uniffiStatus)
		})
		if checksum != 61200 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_passwords_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_registered_services(uniffiStatus)
		})
		if checksum != 46805 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_registered_services: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_sharedstreams_client(uniffiStatus)
		})
		if checksum != 25939 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_sharedstreams_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_get_statuskit_client(uniffiStatus)
		})
		if checksum != 17269 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_get_statuskit_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_init_statuskit(uniffiStatus)
		})
		if checksum != 16074 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_init_statuskit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_invite_to_status_sharing(uniffiStatus)
		})
		if checksum != 40272 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_invite_to_status_sharing: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_list_recoverable_chats(uniffiStatus)
		})
		if checksum != 61049 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_list_recoverable_chats: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_list_recoverable_message_guids(uniffiStatus)
		})
		if checksum != 37296 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_list_recoverable_message_guids: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_purge_recoverable_zones(uniffiStatus)
		})
		if checksum != 38295 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_purge_recoverable_zones: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_reset_cloud_client(uniffiStatus)
		})
		if checksum != 49216 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_reset_cloud_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_reset_sharedstreams_client(uniffiStatus)
		})
		if checksum != 25640 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_reset_sharedstreams_client: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_reset_statuskit_cursors(uniffiStatus)
		})
		if checksum != 35023 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_reset_statuskit_cursors: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_resolve_handle(uniffiStatus)
		})
		if checksum != 29605 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_resolve_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_resolve_handle_cached(uniffiStatus)
		})
		if checksum != 6690 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_resolve_handle_cached: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_restore_cloud_chat(uniffiStatus)
		})
		if checksum != 36876 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_restore_cloud_chat: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_retry_mmcs_from_descriptor(uniffiStatus)
		})
		if checksum != 40462 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_retry_mmcs_from_descriptor: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_attachment(uniffiStatus)
		})
		if checksum != 59726 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_attachment: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_change_participants(uniffiStatus)
		})
		if checksum != 17502 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_change_participants: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_delivery_receipt(uniffiStatus)
		})
		if checksum != 54993 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_delivery_receipt: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_edit(uniffiStatus)
		})
		if checksum != 50609 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_edit: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_error_message(uniffiStatus)
		})
		if checksum != 22923 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_error_message: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_icon_change(uniffiStatus)
		})
		if checksum != 19572 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_icon_change: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_icon_clear(uniffiStatus)
		})
		if checksum != 48413 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_icon_clear: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_mark_unread(uniffiStatus)
		})
		if checksum != 20300 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_mark_unread: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_message(uniffiStatus)
		})
		if checksum != 21078 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_message: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_message_read_on_device(uniffiStatus)
		})
		if checksum != 32416 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_message_read_on_device: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_move_to_recycle_bin(uniffiStatus)
		})
		if checksum != 23546 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_move_to_recycle_bin: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_notify_anyways(uniffiStatus)
		})
		if checksum != 31335 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_notify_anyways: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_peer_cache_invalidate(uniffiStatus)
		})
		if checksum != 62550 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_peer_cache_invalidate: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_permanent_delete_chat(uniffiStatus)
		})
		if checksum != 23579 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_permanent_delete_chat: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_permanent_delete_messages(uniffiStatus)
		})
		if checksum != 40735 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_permanent_delete_messages: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_read_receipt(uniffiStatus)
		})
		if checksum != 61662 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_read_receipt: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_recover_chat(uniffiStatus)
		})
		if checksum != 50565 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_recover_chat: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_rename_group(uniffiStatus)
		})
		if checksum != 8861 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_rename_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_set_transcript_background(uniffiStatus)
		})
		if checksum != 6986 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_set_transcript_background: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_share_profile(uniffiStatus)
		})
		if checksum != 62242 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_share_profile: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_sms_activation(uniffiStatus)
		})
		if checksum != 44855 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_sms_activation: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_sms_confirm_sent(uniffiStatus)
		})
		if checksum != 22079 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_sms_confirm_sent: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_tapback(uniffiStatus)
		})
		if checksum != 6103 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_tapback: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_typing(uniffiStatus)
		})
		if checksum != 5805 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_typing: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_typing_with_app(uniffiStatus)
		})
		if checksum != 8148 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_typing_with_app: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_unschedule(uniffiStatus)
		})
		if checksum != 50141 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_unschedule: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_unsend(uniffiStatus)
		})
		if checksum != 29429 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_unsend: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_update_extension(uniffiStatus)
		})
		if checksum != 23014 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_update_extension: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_update_profile(uniffiStatus)
		})
		if checksum != 42357 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_update_profile: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_send_update_profile_sharing(uniffiStatus)
		})
		if checksum != 10120 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_send_update_profile_sharing: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_set_status(uniffiStatus)
		})
		if checksum != 47775 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_set_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_stop(uniffiStatus)
		})
		if checksum != 26750 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_stop: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_subscribe_to_status(uniffiStatus)
		})
		if checksum != 52856 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_subscribe_to_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_test_cloud_messages(uniffiStatus)
		})
		if checksum != 57936 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_test_cloud_messages: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_unsubscribe_all_status(uniffiStatus)
		})
		if checksum != 47268 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_unsubscribe_all_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_client_validate_targets(uniffiStatus)
		})
		if checksum != 44836 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_client_validate_targets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_loginsession_finish(uniffiStatus)
		})
		if checksum != 25021 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_loginsession_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_loginsession_needs_2fa(uniffiStatus)
		})
		if checksum != 10863 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_loginsession_needs_2fa: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_loginsession_submit_2fa(uniffiStatus)
		})
		if checksum != 25146 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_loginsession_submit_2fa: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedapsconnection_state(uniffiStatus)
		})
		if checksum != 59967 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedapsconnection_state: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedapsstate_to_string(uniffiStatus)
		})
		if checksum != 2386 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedapsstate_to_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_add_members(uniffiStatus)
		})
		if checksum != 4768 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_add_members: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_bind_bridge_link_to_session(uniffiStatus)
		})
		if checksum != 20929 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_bind_bridge_link_to_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_clear_links(uniffiStatus)
		})
		if checksum != 21029 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_clear_links: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_create_session(uniffiStatus)
		})
		if checksum != 12539 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_create_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_create_session_no_ring(uniffiStatus)
		})
		if checksum != 9956 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_create_session_no_ring: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_delete_link(uniffiStatus)
		})
		if checksum != 57656 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_delete_link: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_export_state_json(uniffiStatus)
		})
		if checksum != 63772 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_export_state_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_get_link_for_usage(uniffiStatus)
		})
		if checksum != 56712 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_get_link_for_usage: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_get_session_link(uniffiStatus)
		})
		if checksum != 7494 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_get_session_link: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_list_delegated_letmein_requests(uniffiStatus)
		})
		if checksum != 3880 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_list_delegated_letmein_requests: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_register_pending_ring(uniffiStatus)
		})
		if checksum != 44246 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_register_pending_ring: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_remove_members(uniffiStatus)
		})
		if checksum != 65464 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_remove_members: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_respond_delegated_letmein(uniffiStatus)
		})
		if checksum != 38946 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_respond_delegated_letmein: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_ring(uniffiStatus)
		})
		if checksum != 4043 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_ring: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_use_link_for(uniffiStatus)
		})
		if checksum != 19893 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfacetimeclient_use_link_for: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfindmyclient_accept_item_share(uniffiStatus)
		})
		if checksum != 21451 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfindmyclient_accept_item_share: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfindmyclient_delete_shared_item(uniffiStatus)
		})
		if checksum != 47642 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfindmyclient_delete_shared_item: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfindmyclient_export_state_json(uniffiStatus)
		})
		if checksum != 48669 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfindmyclient_export_state_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfindmyclient_sync_item_positions(uniffiStatus)
		})
		if checksum != 16803 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfindmyclient_sync_item_positions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedfindmyclient_update_beacon_name(uniffiStatus)
		})
		if checksum != 531 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedfindmyclient_update_beacon_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedidsngmidentity_to_string(uniffiStatus)
		})
		if checksum != 19097 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedidsngmidentity_to_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedidsusers_get_handles(uniffiStatus)
		})
		if checksum != 54112 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedidsusers_get_handles: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedidsusers_login_id(uniffiStatus)
		})
		if checksum != 23919 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedidsusers_login_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedidsusers_to_string(uniffiStatus)
		})
		if checksum != 29 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedidsusers_to_string: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedidsusers_validate_keystore(uniffiStatus)
		})
		if checksum != 49609 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedidsusers_validate_keystore: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedosconfig_get_device_id(uniffiStatus)
		})
		if checksum != 39645 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedosconfig_get_device_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedosconfig_requires_nac_relay(uniffiStatus)
		})
		if checksum != 13481 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedosconfig_requires_nac_relay: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_accept_invite(uniffiStatus)
		})
		if checksum != 37840 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_accept_invite: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_create_group(uniffiStatus)
		})
		if checksum != 45849 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_create_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_decline_invite(uniffiStatus)
		})
		if checksum != 48841 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_decline_invite: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_delete_password_raw_entry(uniffiStatus)
		})
		if checksum != 57340 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_delete_password_raw_entry: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_export_state_json(uniffiStatus)
		})
		if checksum != 39354 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_export_state_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_get_password_site_counts(uniffiStatus)
		})
		if checksum != 51887 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_get_password_site_counts: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_invite_user(uniffiStatus)
		})
		if checksum != 44854 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_invite_user: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_list_password_raw_entry_refs(uniffiStatus)
		})
		if checksum != 29676 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_list_password_raw_entry_refs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_query_handle(uniffiStatus)
		})
		if checksum != 11114 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_query_handle: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_remove_group(uniffiStatus)
		})
		if checksum != 10084 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_remove_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_remove_user(uniffiStatus)
		})
		if checksum != 32359 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_remove_user: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_rename_group(uniffiStatus)
		})
		if checksum != 24623 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_rename_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_sync_passwords(uniffiStatus)
		})
		if checksum != 50829 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_sync_passwords: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_upsert_password_raw_entry(uniffiStatus)
		})
		if checksum != 24562 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedpasswordsclient_upsert_password_raw_entry: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_delete_assets(uniffiStatus)
		})
		if checksum != 21365 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_delete_assets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_download_file(uniffiStatus)
		})
		if checksum != 57741 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_download_file: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_export_state_json(uniffiStatus)
		})
		if checksum != 8939 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_export_state_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_album_assets(uniffiStatus)
		})
		if checksum != 12757 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_album_assets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_album_summary(uniffiStatus)
		})
		if checksum != 64494 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_album_summary: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_assets_json(uniffiStatus)
		})
		if checksum != 46429 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_assets_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_changes(uniffiStatus)
		})
		if checksum != 57594 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_get_changes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_list_album_ids(uniffiStatus)
		})
		if checksum != 44544 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_list_album_ids: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_list_albums(uniffiStatus)
		})
		if checksum != 20350 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_list_albums: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_subscribe(uniffiStatus)
		})
		if checksum != 5706 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_subscribe: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_subscribe_token(uniffiStatus)
		})
		if checksum != 37622 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_subscribe_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_unsubscribe(uniffiStatus)
		})
		if checksum != 29614 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedsharedstreamsclient_unsubscribe: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_clear_interest_tokens(uniffiStatus)
		})
		if checksum != 24594 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_clear_interest_tokens: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_export_state_json(uniffiStatus)
		})
		if checksum != 3475 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_export_state_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_get_known_handles(uniffiStatus)
		})
		if checksum != 9296 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_get_known_handles: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_invite_to_channel(uniffiStatus)
		})
		if checksum != 51991 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_invite_to_channel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_request_handles(uniffiStatus)
		})
		if checksum != 17015 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_request_handles: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_reset_keys(uniffiStatus)
		})
		if checksum != 28788 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_reset_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_roll_keys(uniffiStatus)
		})
		if checksum != 23793 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_roll_keys: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_share_status(uniffiStatus)
		})
		if checksum != 10053 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedstatuskitclient_share_status: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_announce_apple_device_if_needed(uniffiStatus)
		})
		if checksum != 60816 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_announce_apple_device_if_needed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_contacts_url(uniffiStatus)
		})
		if checksum != 29421 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_contacts_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_dsid(uniffiStatus)
		})
		if checksum != 58611 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_dsid: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_escrow_devices(uniffiStatus)
		})
		if checksum != 27126 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_escrow_devices: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_icloud_auth_headers(uniffiStatus)
		})
		if checksum != 3524 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_icloud_auth_headers: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_mme_delegate_json(uniffiStatus)
		})
		if checksum != 9782 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_get_mme_delegate_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_join_keychain_clique(uniffiStatus)
		})
		if checksum != 14380 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_join_keychain_clique: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_join_keychain_clique_for_device(uniffiStatus)
		})
		if checksum != 59097 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_join_keychain_clique_for_device: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_refresh_pet_token(uniffiStatus)
		})
		if checksum != 58563 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_refresh_pet_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_wrappedtokenprovider_seed_mme_delegate_json(uniffiStatus)
		})
		if checksum != 6840 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_wrappedtokenprovider_seed_mme_delegate_json: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_constructor_wrappedapsstate_new(uniffiStatus)
		})
		if checksum != 9380 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_constructor_wrappedapsstate_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_constructor_wrappedidsngmidentity_new(uniffiStatus)
		})
		if checksum != 24162 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_constructor_wrappedidsngmidentity_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_constructor_wrappedidsusers_new(uniffiStatus)
		})
		if checksum != 42963 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_constructor_wrappedidsusers_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_messagecallback_on_message(uniffiStatus)
		})
		if checksum != 9227 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_messagecallback_on_message: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_statuscallback_on_status_update(uniffiStatus)
		})
		if checksum != 34109 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_statuscallback_on_status_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_statuscallback_on_keys_received(uniffiStatus)
		})
		if checksum != 53322 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_statuscallback_on_keys_received: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_rustpushgo_checksum_method_updateuserscallback_update_users(uniffiStatus)
		})
		if checksum != 85 {
			// If this happens try cleaning and rebuilding your project
			panic("rustpushgo: uniffi_rustpushgo_checksum_method_updateuserscallback_update_users: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

type FfiConverterInt32 struct{}

var FfiConverterInt32INSTANCE = FfiConverterInt32{}

func (FfiConverterInt32) Lower(value int32) C.int32_t {
	return C.int32_t(value)
}

func (FfiConverterInt32) Write(writer io.Writer, value int32) {
	writeInt32(writer, value)
}

func (FfiConverterInt32) Lift(value C.int32_t) int32 {
	return int32(value)
}

func (FfiConverterInt32) Read(reader io.Reader) int32 {
	return readInt32(reader)
}

type FfiDestroyerInt32 struct{}

func (FfiDestroyerInt32) Destroy(_ int32) {}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterInt64 struct{}

var FfiConverterInt64INSTANCE = FfiConverterInt64{}

func (FfiConverterInt64) Lower(value int64) C.int64_t {
	return C.int64_t(value)
}

func (FfiConverterInt64) Write(writer io.Writer, value int64) {
	writeInt64(writer, value)
}

func (FfiConverterInt64) Lift(value C.int64_t) int64 {
	return int64(value)
}

func (FfiConverterInt64) Read(reader io.Reader) int64 {
	return readInt64(reader)
}

type FfiDestroyerInt64 struct{}

func (FfiDestroyerInt64) Destroy(_ int64) {}

type FfiConverterFloat64 struct{}

var FfiConverterFloat64INSTANCE = FfiConverterFloat64{}

func (FfiConverterFloat64) Lower(value float64) C.double {
	return C.double(value)
}

func (FfiConverterFloat64) Write(writer io.Writer, value float64) {
	writeFloat64(writer, value)
}

func (FfiConverterFloat64) Lift(value C.double) float64 {
	return float64(value)
}

func (FfiConverterFloat64) Read(reader io.Reader) float64 {
	return readFloat64(reader)
}

type FfiDestroyerFloat64 struct{}

func (FfiDestroyerFloat64) Destroy(_ float64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) RustBuffer {
	return stringToRustBuffer(value)
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	pointer      unsafe.Pointer
	callCounter  atomic.Int64
	freeFunction func(unsafe.Pointer, *C.RustCallStatus)
	destroyed    atomic.Bool
}

func newFfiObject(pointer unsafe.Pointer, freeFunction func(unsafe.Pointer, *C.RustCallStatus)) FfiObject {
	return FfiObject{
		pointer:      pointer,
		freeFunction: freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) unsafe.Pointer {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return ffiObject.pointer
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.pointer, status)
		return 0
	})
}

type Client struct {
	ffiObject FfiObject
}

func (_self *Client) CloudDiagFullCount() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_diag_full_count(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudDownloadAttachment(recordName string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_download_attachment(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(recordName)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterBytesINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudDownloadAttachmentAvid(recordName string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_download_attachment_avid(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(recordName)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterBytesINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudDownloadGroupPhoto(recordName string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_download_group_photo(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(recordName)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterBytesINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudDumpChatsJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_dump_chats_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudFetchRecentMessages(sinceTimestampMs uint64, chatId *string, maxPages uint32, maxResults uint32) ([]WrappedCloudSyncMessage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_fetch_recent_messages(
				_pointer, FfiConverterUint64INSTANCE.Lower(sinceTimestampMs), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(chatId)), FfiConverterUint32INSTANCE.Lower(maxPages), FfiConverterUint32INSTANCE.Lower(maxResults),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeWrappedCloudSyncMessageINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudQueryAttachmentsFallback(knownRecordNames []string) (WrappedCloudSyncAttachmentsPage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_query_attachments_fallback(
				_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(knownRecordNames)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedCloudSyncAttachmentsPageINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudSupportsAvidDownload() bool {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_rustpushgo_fn_method_client_cloud_supports_avid_download(
			_pointer, _uniffiStatus)
	}))
}

func (_self *Client) CloudSyncAttachments(continuationToken *string) (WrappedCloudSyncAttachmentsPage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_sync_attachments(
				_pointer, rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(continuationToken)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedCloudSyncAttachmentsPageINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudSyncChats(continuationToken *string) (WrappedCloudSyncChatsPage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_sync_chats(
				_pointer, rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(continuationToken)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedCloudSyncChatsPageINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) CloudSyncMessages(continuationToken *string) (WrappedCloudSyncMessagesPage, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_cloud_sync_messages(
				_pointer, rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(continuationToken)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedCloudSyncMessagesPageINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) DebugRecoverableZones() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_debug_recoverable_zones(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) DeleteCloudChats(chatIds []string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_delete_cloud_chats(
				_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(chatIds)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) DeleteCloudMessages(messageIds []string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_delete_cloud_messages(
				_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(messageIds)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) FetchProfile(recordKey string, decryptionKey []byte, hasPoster bool) (WrappedProfileRecord, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_fetch_profile(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(recordKey)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(decryptionKey)), FfiConverterBoolINSTANCE.Lower(hasPoster),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedProfileRecordINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) FindmyFriendsImport(daemon bool, url string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_findmy_friends_import(
				_pointer, FfiConverterBoolINSTANCE.Lower(daemon), rustBufferToC(FfiConverterStringINSTANCE.Lower(url)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) FindmyFriendsRefreshJson(daemon bool) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_findmy_friends_refresh_json(
				_pointer, FfiConverterBoolINSTANCE.Lower(daemon),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) FindmyPhoneRefreshJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_findmy_phone_refresh_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetContactsUrl() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_contacts_url(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterOptionalStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetDsid() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_dsid(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterOptionalStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetFacetimeClient() (*WrappedFaceTimeClient, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_facetime_client(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedFaceTimeClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetFindmyClient() (*WrappedFindMyClient, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_findmy_client(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedFindMyClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetHandles() []string {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_handles(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetIcloudAuthHeaders() (*map[string]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_icloud_auth_headers(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterOptionalMapStringStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetPasswordsClient() (*WrappedPasswordsClient, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_passwords_client(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedPasswordsClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetRegisteredServices() []string {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_registered_services(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetSharedstreamsClient() (*WrappedSharedStreamsClient, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_sharedstreams_client(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedSharedStreamsClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) GetStatuskitClient() (*WrappedStatusKitClient, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_get_statuskit_client(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedStatusKitClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) InitStatuskit(callback StatusCallback) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_init_statuskit(
				_pointer, FfiConverterCallbackInterfaceStatusCallbackINSTANCE.Lower(callback),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) InviteToStatusSharing(senderHandle string, handles []string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_invite_to_status_sharing(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(senderHandle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(handles)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ListRecoverableChats() ([]WrappedCloudSyncChat, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_list_recoverable_chats(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeWrappedCloudSyncChatINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ListRecoverableMessageGuids() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_list_recoverable_message_guids(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) PurgeRecoverableZones() error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_purge_recoverable_zones(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ResetCloudClient() {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_reset_cloud_client(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ResetSharedstreamsClient() {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_reset_sharedstreams_client(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ResetStatuskitCursors() {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_rustpushgo_fn_method_client_reset_statuskit_cursors(
			_pointer, _uniffiStatus)
		return false
	})
}

func (_self *Client) ResolveHandle(handle string, knownHandles []string) ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_resolve_handle(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(knownHandles)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ResolveHandleCached(handle string, knownHandles []string) []string {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_resolve_handle_cached(
			_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(knownHandles)),
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) RestoreCloudChat(recordName string, chatIdentifier string, groupId string, style int64, service string, displayName *string, participants []string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_restore_cloud_chat(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(recordName)), rustBufferToC(FfiConverterStringINSTANCE.Lower(chatIdentifier)), rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)), FfiConverterInt64INSTANCE.Lower(style), rustBufferToC(FfiConverterStringINSTANCE.Lower(service)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(displayName)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(participants)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) RetryMmcsFromDescriptor(descriptorJson string, name string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_retry_mmcs_from_descriptor(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(descriptorJson)), rustBufferToC(FfiConverterStringINSTANCE.Lower(name)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterBytesINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendAttachment(conversation WrappedConversation, data []byte, mime string, utiType string, filename string, handle string, replyGuid *string, replyPart *string, body *string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_attachment(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(data)), rustBufferToC(FfiConverterStringINSTANCE.Lower(mime)), rustBufferToC(FfiConverterStringINSTANCE.Lower(utiType)), rustBufferToC(FfiConverterStringINSTANCE.Lower(filename)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(replyGuid)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(replyPart)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(body)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendChangeParticipants(conversation WrappedConversation, newParticipants []string, groupVersion uint64, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_change_participants(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(newParticipants)), FfiConverterUint64INSTANCE.Lower(groupVersion), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendDeliveryReceipt(conversation WrappedConversation, handle string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_delivery_receipt(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendEdit(conversation WrappedConversation, targetUuid string, editPart uint64, newText string, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_edit(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(targetUuid)), FfiConverterUint64INSTANCE.Lower(editPart), rustBufferToC(FfiConverterStringINSTANCE.Lower(newText)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendErrorMessage(conversation WrappedConversation, forUuid string, errorStatus uint64, statusStr string, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_error_message(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(forUuid)), FfiConverterUint64INSTANCE.Lower(errorStatus), rustBufferToC(FfiConverterStringINSTANCE.Lower(statusStr)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendIconChange(conversation WrappedConversation, photoData []byte, groupVersion uint64, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_icon_change(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(photoData)), FfiConverterUint64INSTANCE.Lower(groupVersion), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendIconClear(conversation WrappedConversation, groupVersion uint64, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_icon_clear(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterUint64INSTANCE.Lower(groupVersion), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendMarkUnread(conversation WrappedConversation, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_mark_unread(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendMessage(conversation WrappedConversation, text string, html *string, handle string, replyGuid *string, replyPart *string, scheduledMs *uint64) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_message(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(text)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(html)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(replyGuid)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(replyPart)), rustBufferToC(FfiConverterOptionalUint64INSTANCE.Lower(scheduledMs)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendMessageReadOnDevice(conversation WrappedConversation, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_message_read_on_device(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendMoveToRecycleBin(conversation WrappedConversation, handle string, chatGuid string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_move_to_recycle_bin(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterStringINSTANCE.Lower(chatGuid)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendNotifyAnyways(conversation WrappedConversation, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_notify_anyways(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendPeerCacheInvalidate(conversation WrappedConversation, handle string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_peer_cache_invalidate(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendPermanentDeleteChat(conversation WrappedConversation, chatGuid string, isScheduled bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_permanent_delete_chat(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(chatGuid)), FfiConverterBoolINSTANCE.Lower(isScheduled), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendPermanentDeleteMessages(conversation WrappedConversation, messageUuids []string, isScheduled bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_permanent_delete_messages(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(messageUuids)), FfiConverterBoolINSTANCE.Lower(isScheduled), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendReadReceipt(conversation WrappedConversation, handle string, forUuid *string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_read_receipt(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(forUuid)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendRecoverChat(conversation WrappedConversation, handle string, chatGuid string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_recover_chat(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterStringINSTANCE.Lower(chatGuid)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendRenameGroup(conversation WrappedConversation, newName string, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_rename_group(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(newName)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendSetTranscriptBackground(conversation WrappedConversation, groupVersion uint64, imageData *[]byte, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_set_transcript_background(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterUint64INSTANCE.Lower(groupVersion), rustBufferToC(FfiConverterOptionalBytesINSTANCE.Lower(imageData)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendShareProfile(conversation WrappedConversation, cloudKitRecordKey string, cloudKitDecryptionRecordKey []byte, lowResWallpaperTag *[]byte, wallpaperTag *[]byte, messageTag *[]byte, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_share_profile(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(cloudKitRecordKey)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(cloudKitDecryptionRecordKey)), rustBufferToC(FfiConverterOptionalBytesINSTANCE.Lower(lowResWallpaperTag)), rustBufferToC(FfiConverterOptionalBytesINSTANCE.Lower(wallpaperTag)), rustBufferToC(FfiConverterOptionalBytesINSTANCE.Lower(messageTag)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendSmsActivation(conversation WrappedConversation, enable bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_sms_activation(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterBoolINSTANCE.Lower(enable), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendSmsConfirmSent(conversation WrappedConversation, smsStatus bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_sms_confirm_sent(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterBoolINSTANCE.Lower(smsStatus), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendTapback(conversation WrappedConversation, targetUuid string, targetPart uint64, reaction uint32, emoji *string, remove bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_tapback(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(targetUuid)), FfiConverterUint64INSTANCE.Lower(targetPart), FfiConverterUint32INSTANCE.Lower(reaction), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(emoji)), FfiConverterBoolINSTANCE.Lower(remove), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendTyping(conversation WrappedConversation, typing bool, handle string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_typing(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterBoolINSTANCE.Lower(typing), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendTypingWithApp(conversation WrappedConversation, typing bool, handle string, bundleId string, icon []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_typing_with_app(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), FfiConverterBoolINSTANCE.Lower(typing), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterStringINSTANCE.Lower(bundleId)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(icon)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendUnschedule(conversation WrappedConversation, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_unschedule(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendUnsend(conversation WrappedConversation, targetUuid string, editPart uint64, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_unsend(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(targetUuid)), FfiConverterUint64INSTANCE.Lower(editPart), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendUpdateExtension(conversation WrappedConversation, forUuid string, extension WrappedStickerExtension, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_update_extension(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterStringINSTANCE.Lower(forUuid)), rustBufferToC(FfiConverterTypeWrappedStickerExtensionINSTANCE.Lower(extension)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendUpdateProfile(conversation WrappedConversation, profile *WrappedShareProfileData, shareContacts bool, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_update_profile(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterOptionalTypeWrappedShareProfileDataINSTANCE.Lower(profile)), FfiConverterBoolINSTANCE.Lower(shareContacts), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SendUpdateProfileSharing(conversation WrappedConversation, sharedDismissed []string, sharedAll []string, version uint64, handle string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_send_update_profile_sharing(
				_pointer, rustBufferToC(FfiConverterTypeWrappedConversationINSTANCE.Lower(conversation)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(sharedDismissed)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(sharedAll)), FfiConverterUint64INSTANCE.Lower(version), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SetStatus(active bool) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_set_status(
				_pointer, FfiConverterBoolINSTANCE.Lower(active),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) Stop() {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_stop(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) SubscribeToStatus(handles []string) error {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_subscribe_to_status(
				_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(handles)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) TestCloudMessages() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_client_test_cloud_messages(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) UnsubscribeAllStatus() {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_unsubscribe_all_status(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *Client) ValidateTargets(targets []string, handle string) []string {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_client_validate_targets(
			_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(targets)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (object *Client) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterClient struct{}

var FfiConverterClientINSTANCE = FfiConverterClient{}

func (c FfiConverterClient) Lift(pointer unsafe.Pointer) *Client {
	result := &Client{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_client(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*Client).Destroy)
	return result
}

func (c FfiConverterClient) Read(reader io.Reader) *Client {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterClient) Lower(value *Client) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*Client")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterClient) Write(writer io.Writer, value *Client) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerClient struct{}

func (_ FfiDestroyerClient) Destroy(value *Client) {
	value.Destroy()
}

type LoginSession struct {
	ffiObject FfiObject
}

func (_self *LoginSession) Finish(config *WrappedOsConfig, connection *WrappedApsConnection, existingIdentity **WrappedIdsngmIdentity, existingUsers **WrappedIdsUsers) (IdsUsersWithIdentityRecord, error) {
	_pointer := _self.ffiObject.incrementPointer("*LoginSession")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_loginsession_finish(
				_pointer, FfiConverterWrappedOSConfigINSTANCE.Lower(config), FfiConverterWrappedAPSConnectionINSTANCE.Lower(connection), rustBufferToC(FfiConverterOptionalWrappedIDSNGMIdentityINSTANCE.Lower(existingIdentity)), rustBufferToC(FfiConverterOptionalWrappedIDSUsersINSTANCE.Lower(existingUsers)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeIDSUsersWithIdentityRecordINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *LoginSession) Needs2fa() bool {
	_pointer := _self.ffiObject.incrementPointer("*LoginSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_rustpushgo_fn_method_loginsession_needs_2fa(
			_pointer, _uniffiStatus)
	}))
}

func (_self *LoginSession) Submit2fa(code string) (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*LoginSession")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_loginsession_submit_2fa(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(code)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_i8(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) C.int8_t {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_i8(unsafe.Pointer(handle), status)
		},
		FfiConverterBoolINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_i8(unsafe.Pointer(rustFuture), status)
		})
}

func (object *LoginSession) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterLoginSession struct{}

var FfiConverterLoginSessionINSTANCE = FfiConverterLoginSession{}

func (c FfiConverterLoginSession) Lift(pointer unsafe.Pointer) *LoginSession {
	result := &LoginSession{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_loginsession(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*LoginSession).Destroy)
	return result
}

func (c FfiConverterLoginSession) Read(reader io.Reader) *LoginSession {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterLoginSession) Lower(value *LoginSession) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*LoginSession")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterLoginSession) Write(writer io.Writer, value *LoginSession) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerLoginSession struct{}

func (_ FfiDestroyerLoginSession) Destroy(value *LoginSession) {
	value.Destroy()
}

type WrappedApsConnection struct {
	ffiObject FfiObject
}

func (_self *WrappedApsConnection) State() *WrappedApsState {
	_pointer := _self.ffiObject.incrementPointer("*WrappedApsConnection")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedapsconnection_state(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedAPSStateINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedApsConnection) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedAPSConnection struct{}

var FfiConverterWrappedAPSConnectionINSTANCE = FfiConverterWrappedAPSConnection{}

func (c FfiConverterWrappedAPSConnection) Lift(pointer unsafe.Pointer) *WrappedApsConnection {
	result := &WrappedApsConnection{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedapsconnection(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedApsConnection).Destroy)
	return result
}

func (c FfiConverterWrappedAPSConnection) Read(reader io.Reader) *WrappedApsConnection {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedAPSConnection) Lower(value *WrappedApsConnection) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedApsConnection")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedAPSConnection) Write(writer io.Writer, value *WrappedApsConnection) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedApsConnection struct{}

func (_ FfiDestroyerWrappedApsConnection) Destroy(value *WrappedApsConnection) {
	value.Destroy()
}

type WrappedApsState struct {
	ffiObject FfiObject
}

func NewWrappedApsState(string *string) *WrappedApsState {
	return FfiConverterWrappedAPSStateINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_constructor_wrappedapsstate_new(rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(string)), _uniffiStatus)
	}))
}

func (_self *WrappedApsState) ToString() string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedApsState")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedapsstate_to_string(
			_pointer, _uniffiStatus))
	}))
}

func (object *WrappedApsState) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedAPSState struct{}

var FfiConverterWrappedAPSStateINSTANCE = FfiConverterWrappedAPSState{}

func (c FfiConverterWrappedAPSState) Lift(pointer unsafe.Pointer) *WrappedApsState {
	result := &WrappedApsState{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedapsstate(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedApsState).Destroy)
	return result
}

func (c FfiConverterWrappedAPSState) Read(reader io.Reader) *WrappedApsState {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedAPSState) Lower(value *WrappedApsState) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedApsState")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedAPSState) Write(writer io.Writer, value *WrappedApsState) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedApsState struct{}

func (_ FfiDestroyerWrappedApsState) Destroy(value *WrappedApsState) {
	value.Destroy()
}

type WrappedFaceTimeClient struct {
	ffiObject FfiObject
}

func (_self *WrappedFaceTimeClient) AddMembers(sessionId string, handles []string, letmein bool, toMembers *[]string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_add_members(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(sessionId)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(handles)), FfiConverterBoolINSTANCE.Lower(letmein), rustBufferToC(FfiConverterOptionalSequenceStringINSTANCE.Lower(toMembers)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) BindBridgeLinkToSession(handle string, usage string, groupId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_bind_bridge_link_to_session(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterStringINSTANCE.Lower(usage)), rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) ClearLinks() error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_clear_links(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) CreateSession(groupId string, handle string, participants []string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_create_session(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(participants)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) CreateSessionNoRing(groupId string, handle string, participants []string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_create_session_no_ring(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(participants)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) DeleteLink(pseud string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_delete_link(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(pseud)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) ExportStateJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_export_state_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) GetLinkForUsage(handle string, usage string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_get_link_for_usage(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)), rustBufferToC(FfiConverterStringINSTANCE.Lower(usage)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) GetSessionLink(guid string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_get_session_link(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(guid)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) ListDelegatedLetmeinRequests() []WrappedLetMeInRequest {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_list_delegated_letmein_requests(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeWrappedLetMeInRequestINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) RegisterPendingRing(sessionId string, callerHandle string, targets []string, ttlSecs uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_register_pending_ring(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(sessionId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(callerHandle)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(targets)), FfiConverterUint64INSTANCE.Lower(ttlSecs),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) RemoveMembers(sessionId string, handles []string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_remove_members(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(sessionId)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(handles)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) RespondDelegatedLetmein(delegationUuid string, approvedGroup *string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_respond_delegated_letmein(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(delegationUuid)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(approvedGroup)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) Ring(sessionId string, targets []string, letmein bool) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_ring(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(sessionId)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(targets)), FfiConverterBoolINSTANCE.Lower(letmein),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFaceTimeClient) UseLinkFor(oldUsage string, usage string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfacetimeclient_use_link_for(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(oldUsage)), rustBufferToC(FfiConverterStringINSTANCE.Lower(usage)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedFaceTimeClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedFaceTimeClient struct{}

var FfiConverterWrappedFaceTimeClientINSTANCE = FfiConverterWrappedFaceTimeClient{}

func (c FfiConverterWrappedFaceTimeClient) Lift(pointer unsafe.Pointer) *WrappedFaceTimeClient {
	result := &WrappedFaceTimeClient{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedfacetimeclient(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedFaceTimeClient).Destroy)
	return result
}

func (c FfiConverterWrappedFaceTimeClient) Read(reader io.Reader) *WrappedFaceTimeClient {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedFaceTimeClient) Lower(value *WrappedFaceTimeClient) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedFaceTimeClient")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedFaceTimeClient) Write(writer io.Writer, value *WrappedFaceTimeClient) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedFaceTimeClient struct{}

func (_ FfiDestroyerWrappedFaceTimeClient) Destroy(value *WrappedFaceTimeClient) {
	value.Destroy()
}

type WrappedFindMyClient struct {
	ffiObject FfiObject
}

func (_self *WrappedFindMyClient) AcceptItemShare(circleId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfindmyclient_accept_item_share(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(circleId)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFindMyClient) DeleteSharedItem(id string, removeBeacon bool) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfindmyclient_delete_shared_item(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(id)), FfiConverterBoolINSTANCE.Lower(removeBeacon),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFindMyClient) ExportStateJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfindmyclient_export_state_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFindMyClient) SyncItemPositions() error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfindmyclient_sync_item_positions(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedFindMyClient) UpdateBeaconName(associatedBeacon string, roleId int64, name string, emoji string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedfindmyclient_update_beacon_name(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(associatedBeacon)), FfiConverterInt64INSTANCE.Lower(roleId), rustBufferToC(FfiConverterStringINSTANCE.Lower(name)), rustBufferToC(FfiConverterStringINSTANCE.Lower(emoji)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedFindMyClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedFindMyClient struct{}

var FfiConverterWrappedFindMyClientINSTANCE = FfiConverterWrappedFindMyClient{}

func (c FfiConverterWrappedFindMyClient) Lift(pointer unsafe.Pointer) *WrappedFindMyClient {
	result := &WrappedFindMyClient{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedfindmyclient(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedFindMyClient).Destroy)
	return result
}

func (c FfiConverterWrappedFindMyClient) Read(reader io.Reader) *WrappedFindMyClient {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedFindMyClient) Lower(value *WrappedFindMyClient) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedFindMyClient")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedFindMyClient) Write(writer io.Writer, value *WrappedFindMyClient) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedFindMyClient struct{}

func (_ FfiDestroyerWrappedFindMyClient) Destroy(value *WrappedFindMyClient) {
	value.Destroy()
}

type WrappedIdsngmIdentity struct {
	ffiObject FfiObject
}

func NewWrappedIdsngmIdentity(string *string) *WrappedIdsngmIdentity {
	return FfiConverterWrappedIDSNGMIdentityINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_constructor_wrappedidsngmidentity_new(rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(string)), _uniffiStatus)
	}))
}

func (_self *WrappedIdsngmIdentity) ToString() string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedIdsngmIdentity")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedidsngmidentity_to_string(
			_pointer, _uniffiStatus))
	}))
}

func (object *WrappedIdsngmIdentity) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedIDSNGMIdentity struct{}

var FfiConverterWrappedIDSNGMIdentityINSTANCE = FfiConverterWrappedIDSNGMIdentity{}

func (c FfiConverterWrappedIDSNGMIdentity) Lift(pointer unsafe.Pointer) *WrappedIdsngmIdentity {
	result := &WrappedIdsngmIdentity{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedidsngmidentity(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedIdsngmIdentity).Destroy)
	return result
}

func (c FfiConverterWrappedIDSNGMIdentity) Read(reader io.Reader) *WrappedIdsngmIdentity {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedIDSNGMIdentity) Lower(value *WrappedIdsngmIdentity) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedIdsngmIdentity")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedIDSNGMIdentity) Write(writer io.Writer, value *WrappedIdsngmIdentity) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedIdsngmIdentity struct{}

func (_ FfiDestroyerWrappedIdsngmIdentity) Destroy(value *WrappedIdsngmIdentity) {
	value.Destroy()
}

type WrappedIdsUsers struct {
	ffiObject FfiObject
}

func NewWrappedIdsUsers(string *string) *WrappedIdsUsers {
	return FfiConverterWrappedIDSUsersINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_constructor_wrappedidsusers_new(rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(string)), _uniffiStatus)
	}))
}

func (_self *WrappedIdsUsers) GetHandles() []string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedIdsUsers")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterSequenceStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedidsusers_get_handles(
			_pointer, _uniffiStatus))
	}))
}

func (_self *WrappedIdsUsers) LoginId(i uint64) string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedIdsUsers")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedidsusers_login_id(
			_pointer, FfiConverterUint64INSTANCE.Lower(i), _uniffiStatus))
	}))
}

func (_self *WrappedIdsUsers) ToString() string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedIdsUsers")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedidsusers_to_string(
			_pointer, _uniffiStatus))
	}))
}

func (_self *WrappedIdsUsers) ValidateKeystore() bool {
	_pointer := _self.ffiObject.incrementPointer("*WrappedIdsUsers")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_rustpushgo_fn_method_wrappedidsusers_validate_keystore(
			_pointer, _uniffiStatus)
	}))
}

func (object *WrappedIdsUsers) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedIDSUsers struct{}

var FfiConverterWrappedIDSUsersINSTANCE = FfiConverterWrappedIDSUsers{}

func (c FfiConverterWrappedIDSUsers) Lift(pointer unsafe.Pointer) *WrappedIdsUsers {
	result := &WrappedIdsUsers{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedidsusers(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedIdsUsers).Destroy)
	return result
}

func (c FfiConverterWrappedIDSUsers) Read(reader io.Reader) *WrappedIdsUsers {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedIDSUsers) Lower(value *WrappedIdsUsers) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedIdsUsers")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedIDSUsers) Write(writer io.Writer, value *WrappedIdsUsers) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedIdsUsers struct{}

func (_ FfiDestroyerWrappedIdsUsers) Destroy(value *WrappedIdsUsers) {
	value.Destroy()
}

type WrappedOsConfig struct {
	ffiObject FfiObject
}

func (_self *WrappedOsConfig) GetDeviceId() string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedOsConfig")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return rustBufferFromC(C.uniffi_rustpushgo_fn_method_wrappedosconfig_get_device_id(
			_pointer, _uniffiStatus))
	}))
}

func (_self *WrappedOsConfig) RequiresNacRelay() bool {
	_pointer := _self.ffiObject.incrementPointer("*WrappedOsConfig")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterBoolINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.int8_t {
		return C.uniffi_rustpushgo_fn_method_wrappedosconfig_requires_nac_relay(
			_pointer, _uniffiStatus)
	}))
}

func (object *WrappedOsConfig) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedOSConfig struct{}

var FfiConverterWrappedOSConfigINSTANCE = FfiConverterWrappedOSConfig{}

func (c FfiConverterWrappedOSConfig) Lift(pointer unsafe.Pointer) *WrappedOsConfig {
	result := &WrappedOsConfig{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedosconfig(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedOsConfig).Destroy)
	return result
}

func (c FfiConverterWrappedOSConfig) Read(reader io.Reader) *WrappedOsConfig {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedOSConfig) Lower(value *WrappedOsConfig) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedOsConfig")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedOSConfig) Write(writer io.Writer, value *WrappedOsConfig) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedOsConfig struct{}

func (_ FfiDestroyerWrappedOsConfig) Destroy(value *WrappedOsConfig) {
	value.Destroy()
}

type WrappedPasswordsClient struct {
	ffiObject FfiObject
}

func (_self *WrappedPasswordsClient) AcceptInvite(inviteId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_accept_invite(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(inviteId)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) CreateGroup(name string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_create_group(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(name)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) DeclineInvite(inviteId string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_decline_invite(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(inviteId)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) DeletePasswordRawEntry(id string, group *string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_delete_password_raw_entry(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(id)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(group)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) ExportStateJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_export_state_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) GetPasswordSiteCounts(site string) WrappedPasswordSiteCounts {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_get_password_site_counts(
			_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(site)),
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterTypeWrappedPasswordSiteCountsINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) InviteUser(groupId string, handle string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_invite_user(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) ListPasswordRawEntryRefs() []WrappedPasswordEntryRef {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_list_password_raw_entry_refs(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeWrappedPasswordEntryRefINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) QueryHandle(handle string) (bool, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_query_handle(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_i8(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) C.int8_t {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_i8(unsafe.Pointer(handle), status)
		},
		FfiConverterBoolINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_i8(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) RemoveGroup(id string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_remove_group(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(id)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) RemoveUser(groupId string, handle string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_remove_user(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(groupId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(handle)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) RenameGroup(id string, newName string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_rename_group(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(id)), rustBufferToC(FfiConverterStringINSTANCE.Lower(newName)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) SyncPasswords() error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_sync_passwords(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedPasswordsClient) UpsertPasswordRawEntry(id string, site string, account string, secretData []byte, group *string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedpasswordsclient_upsert_password_raw_entry(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(id)), rustBufferToC(FfiConverterStringINSTANCE.Lower(site)), rustBufferToC(FfiConverterStringINSTANCE.Lower(account)), rustBufferToC(FfiConverterBytesINSTANCE.Lower(secretData)), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(group)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedPasswordsClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedPasswordsClient struct{}

var FfiConverterWrappedPasswordsClientINSTANCE = FfiConverterWrappedPasswordsClient{}

func (c FfiConverterWrappedPasswordsClient) Lift(pointer unsafe.Pointer) *WrappedPasswordsClient {
	result := &WrappedPasswordsClient{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedpasswordsclient(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedPasswordsClient).Destroy)
	return result
}

func (c FfiConverterWrappedPasswordsClient) Read(reader io.Reader) *WrappedPasswordsClient {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedPasswordsClient) Lower(value *WrappedPasswordsClient) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedPasswordsClient")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedPasswordsClient) Write(writer io.Writer, value *WrappedPasswordsClient) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedPasswordsClient struct{}

func (_ FfiDestroyerWrappedPasswordsClient) Destroy(value *WrappedPasswordsClient) {
	value.Destroy()
}

type WrappedSharedStreamsClient struct {
	ffiObject FfiObject
}

func (_self *WrappedSharedStreamsClient) DeleteAssets(album string, assets []string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_delete_assets(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(assets)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) DownloadFile(album string, assetGuid string) ([]byte, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_download_file(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)), rustBufferToC(FfiConverterStringINSTANCE.Lower(assetGuid)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterBytesINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) ExportStateJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_export_state_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) GetAlbumAssets(album string) ([]SharedAssetInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_get_album_assets(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeSharedAssetInfoINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) GetAlbumSummary(album string) ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_get_album_summary(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) GetAssetsJson(album string, assets []string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_get_assets_json(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)), rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(assets)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) GetChanges() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_get_changes(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) ListAlbumIds() []string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_list_album_ids(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) ListAlbums() []SharedAlbumInfo {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_list_albums(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeSharedAlbumInfoINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) Subscribe(album string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_subscribe(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) SubscribeToken(token string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_subscribe_token(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(token)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedSharedStreamsClient) Unsubscribe(album string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedsharedstreamsclient_unsubscribe(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(album)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedSharedStreamsClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedSharedStreamsClient struct{}

var FfiConverterWrappedSharedStreamsClientINSTANCE = FfiConverterWrappedSharedStreamsClient{}

func (c FfiConverterWrappedSharedStreamsClient) Lift(pointer unsafe.Pointer) *WrappedSharedStreamsClient {
	result := &WrappedSharedStreamsClient{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedsharedstreamsclient(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedSharedStreamsClient).Destroy)
	return result
}

func (c FfiConverterWrappedSharedStreamsClient) Read(reader io.Reader) *WrappedSharedStreamsClient {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedSharedStreamsClient) Lower(value *WrappedSharedStreamsClient) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedSharedStreamsClient")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedSharedStreamsClient) Write(writer io.Writer, value *WrappedSharedStreamsClient) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedSharedStreamsClient struct{}

func (_ FfiDestroyerWrappedSharedStreamsClient) Destroy(value *WrappedSharedStreamsClient) {
	value.Destroy()
}

type WrappedStatusKitClient struct {
	ffiObject FfiObject
}

func (_self *WrappedStatusKitClient) ClearInterestTokens() {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_clear_interest_tokens(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) ExportStateJson() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_export_state_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) GetKnownHandles() []string {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_get_known_handles(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) InviteToChannel(senderHandle string, handles []WrappedStatusKitInviteHandle) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_invite_to_channel(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(senderHandle)), rustBufferToC(FfiConverterSequenceTypeWrappedStatusKitInviteHandleINSTANCE.Lower(handles)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) RequestHandles(handles []string) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_request_handles(
			_pointer, rustBufferToC(FfiConverterSequenceStringINSTANCE.Lower(handles)),
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) ResetKeys() {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_reset_keys(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) RollKeys() {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	uniffiRustCallAsync(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_roll_keys(
			_pointer,
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedStatusKitClient) ShareStatus(active bool, mode *string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedstatuskitclient_share_status(
				_pointer, FfiConverterBoolINSTANCE.Lower(active), rustBufferToC(FfiConverterOptionalStringINSTANCE.Lower(mode)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedStatusKitClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedStatusKitClient struct{}

var FfiConverterWrappedStatusKitClientINSTANCE = FfiConverterWrappedStatusKitClient{}

func (c FfiConverterWrappedStatusKitClient) Lift(pointer unsafe.Pointer) *WrappedStatusKitClient {
	result := &WrappedStatusKitClient{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedstatuskitclient(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedStatusKitClient).Destroy)
	return result
}

func (c FfiConverterWrappedStatusKitClient) Read(reader io.Reader) *WrappedStatusKitClient {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedStatusKitClient) Lower(value *WrappedStatusKitClient) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedStatusKitClient")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedStatusKitClient) Write(writer io.Writer, value *WrappedStatusKitClient) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedStatusKitClient struct{}

func (_ FfiDestroyerWrappedStatusKitClient) Destroy(value *WrappedStatusKitClient) {
	value.Destroy()
}

type WrappedTokenProvider struct {
	ffiObject FfiObject
}

func (_self *WrappedTokenProvider) AnnounceAppleDeviceIfNeeded() error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_announce_apple_device_if_needed(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) GetContactsUrl() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_get_contacts_url(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterOptionalStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) GetDsid() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_get_dsid(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) GetEscrowDevices() ([]EscrowDeviceInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_get_escrow_devices(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterSequenceTypeEscrowDeviceInfoINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) GetIcloudAuthHeaders() (map[string]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_get_icloud_auth_headers(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterMapStringStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) GetMmeDelegateJson() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_get_mme_delegate_json(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterOptionalStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) JoinKeychainClique(passcode string) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_join_keychain_clique(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(passcode)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) JoinKeychainCliqueForDevice(passcode string, deviceIndex uint32) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_join_keychain_clique_for_device(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(passcode)), FfiConverterUint32INSTANCE.Lower(deviceIndex),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_rust_buffer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) RustBufferI {
			// completeFunc
			return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))
		},
		FfiConverterStringINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_rust_buffer(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) RefreshPetToken() error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_refresh_pet_token(
				_pointer,
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (_self *WrappedTokenProvider) SeedMmeDelegateJson(json string) error {
	_pointer := _self.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer _self.ffiObject.decrementPointer()
	return uniffiRustCallAsyncWithError(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_method_wrappedtokenprovider_seed_mme_delegate_json(
				_pointer, rustBufferToC(FfiConverterStringINSTANCE.Lower(json)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_void(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) {
			// completeFunc
			C.ffi_rustpushgo_rust_future_complete_void(unsafe.Pointer(handle), status)
		},
		func(bool) {}, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_void(unsafe.Pointer(rustFuture), status)
		})
}

func (object *WrappedTokenProvider) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterWrappedTokenProvider struct{}

var FfiConverterWrappedTokenProviderINSTANCE = FfiConverterWrappedTokenProvider{}

func (c FfiConverterWrappedTokenProvider) Lift(pointer unsafe.Pointer) *WrappedTokenProvider {
	result := &WrappedTokenProvider{
		newFfiObject(
			pointer,
			func(pointer unsafe.Pointer, status *C.RustCallStatus) {
				C.uniffi_rustpushgo_fn_free_wrappedtokenprovider(pointer, status)
			}),
	}
	runtime.SetFinalizer(result, (*WrappedTokenProvider).Destroy)
	return result
}

func (c FfiConverterWrappedTokenProvider) Read(reader io.Reader) *WrappedTokenProvider {
	return c.Lift(unsafe.Pointer(uintptr(readUint64(reader))))
}

func (c FfiConverterWrappedTokenProvider) Lower(value *WrappedTokenProvider) unsafe.Pointer {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the pointer will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked pointer.
	pointer := value.ffiObject.incrementPointer("*WrappedTokenProvider")
	defer value.ffiObject.decrementPointer()
	return pointer
}

func (c FfiConverterWrappedTokenProvider) Write(writer io.Writer, value *WrappedTokenProvider) {
	writeUint64(writer, uint64(uintptr(c.Lower(value))))
}

type FfiDestroyerWrappedTokenProvider struct{}

func (_ FfiDestroyerWrappedTokenProvider) Destroy(value *WrappedTokenProvider) {
	value.Destroy()
}

type AccountPersistData struct {
	Username          string
	HashedPasswordHex string
	Pet               string
	Adsid             string
	Dsid              string
	SpdBase64         string
}

func (r *AccountPersistData) Destroy() {
	FfiDestroyerString{}.Destroy(r.Username)
	FfiDestroyerString{}.Destroy(r.HashedPasswordHex)
	FfiDestroyerString{}.Destroy(r.Pet)
	FfiDestroyerString{}.Destroy(r.Adsid)
	FfiDestroyerString{}.Destroy(r.Dsid)
	FfiDestroyerString{}.Destroy(r.SpdBase64)
}

type FfiConverterTypeAccountPersistData struct{}

var FfiConverterTypeAccountPersistDataINSTANCE = FfiConverterTypeAccountPersistData{}

func (c FfiConverterTypeAccountPersistData) Lift(rb RustBufferI) AccountPersistData {
	return LiftFromRustBuffer[AccountPersistData](c, rb)
}

func (c FfiConverterTypeAccountPersistData) Read(reader io.Reader) AccountPersistData {
	return AccountPersistData{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeAccountPersistData) Lower(value AccountPersistData) RustBuffer {
	return LowerIntoRustBuffer[AccountPersistData](c, value)
}

func (c FfiConverterTypeAccountPersistData) Write(writer io.Writer, value AccountPersistData) {
	FfiConverterStringINSTANCE.Write(writer, value.Username)
	FfiConverterStringINSTANCE.Write(writer, value.HashedPasswordHex)
	FfiConverterStringINSTANCE.Write(writer, value.Pet)
	FfiConverterStringINSTANCE.Write(writer, value.Adsid)
	FfiConverterStringINSTANCE.Write(writer, value.Dsid)
	FfiConverterStringINSTANCE.Write(writer, value.SpdBase64)
}

type FfiDestroyerTypeAccountPersistData struct{}

func (_ FfiDestroyerTypeAccountPersistData) Destroy(value AccountPersistData) {
	value.Destroy()
}

type EscrowDeviceInfo struct {
	Index       uint32
	DeviceName  string
	DeviceModel string
	Serial      string
	Timestamp   string
}

func (r *EscrowDeviceInfo) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.Index)
	FfiDestroyerString{}.Destroy(r.DeviceName)
	FfiDestroyerString{}.Destroy(r.DeviceModel)
	FfiDestroyerString{}.Destroy(r.Serial)
	FfiDestroyerString{}.Destroy(r.Timestamp)
}

type FfiConverterTypeEscrowDeviceInfo struct{}

var FfiConverterTypeEscrowDeviceInfoINSTANCE = FfiConverterTypeEscrowDeviceInfo{}

func (c FfiConverterTypeEscrowDeviceInfo) Lift(rb RustBufferI) EscrowDeviceInfo {
	return LiftFromRustBuffer[EscrowDeviceInfo](c, rb)
}

func (c FfiConverterTypeEscrowDeviceInfo) Read(reader io.Reader) EscrowDeviceInfo {
	return EscrowDeviceInfo{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeEscrowDeviceInfo) Lower(value EscrowDeviceInfo) RustBuffer {
	return LowerIntoRustBuffer[EscrowDeviceInfo](c, value)
}

func (c FfiConverterTypeEscrowDeviceInfo) Write(writer io.Writer, value EscrowDeviceInfo) {
	FfiConverterUint32INSTANCE.Write(writer, value.Index)
	FfiConverterStringINSTANCE.Write(writer, value.DeviceName)
	FfiConverterStringINSTANCE.Write(writer, value.DeviceModel)
	FfiConverterStringINSTANCE.Write(writer, value.Serial)
	FfiConverterStringINSTANCE.Write(writer, value.Timestamp)
}

type FfiDestroyerTypeEscrowDeviceInfo struct{}

func (_ FfiDestroyerTypeEscrowDeviceInfo) Destroy(value EscrowDeviceInfo) {
	value.Destroy()
}

type IdsUsersWithIdentityRecord struct {
	Users          *WrappedIdsUsers
	Identity       *WrappedIdsngmIdentity
	TokenProvider  **WrappedTokenProvider
	AccountPersist *AccountPersistData
}

func (r *IdsUsersWithIdentityRecord) Destroy() {
	FfiDestroyerWrappedIdsUsers{}.Destroy(r.Users)
	FfiDestroyerWrappedIdsngmIdentity{}.Destroy(r.Identity)
	FfiDestroyerOptionalWrappedTokenProvider{}.Destroy(r.TokenProvider)
	FfiDestroyerOptionalTypeAccountPersistData{}.Destroy(r.AccountPersist)
}

type FfiConverterTypeIDSUsersWithIdentityRecord struct{}

var FfiConverterTypeIDSUsersWithIdentityRecordINSTANCE = FfiConverterTypeIDSUsersWithIdentityRecord{}

func (c FfiConverterTypeIDSUsersWithIdentityRecord) Lift(rb RustBufferI) IdsUsersWithIdentityRecord {
	return LiftFromRustBuffer[IdsUsersWithIdentityRecord](c, rb)
}

func (c FfiConverterTypeIDSUsersWithIdentityRecord) Read(reader io.Reader) IdsUsersWithIdentityRecord {
	return IdsUsersWithIdentityRecord{
		FfiConverterWrappedIDSUsersINSTANCE.Read(reader),
		FfiConverterWrappedIDSNGMIdentityINSTANCE.Read(reader),
		FfiConverterOptionalWrappedTokenProviderINSTANCE.Read(reader),
		FfiConverterOptionalTypeAccountPersistDataINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeIDSUsersWithIdentityRecord) Lower(value IdsUsersWithIdentityRecord) RustBuffer {
	return LowerIntoRustBuffer[IdsUsersWithIdentityRecord](c, value)
}

func (c FfiConverterTypeIDSUsersWithIdentityRecord) Write(writer io.Writer, value IdsUsersWithIdentityRecord) {
	FfiConverterWrappedIDSUsersINSTANCE.Write(writer, value.Users)
	FfiConverterWrappedIDSNGMIdentityINSTANCE.Write(writer, value.Identity)
	FfiConverterOptionalWrappedTokenProviderINSTANCE.Write(writer, value.TokenProvider)
	FfiConverterOptionalTypeAccountPersistDataINSTANCE.Write(writer, value.AccountPersist)
}

type FfiDestroyerTypeIdsUsersWithIdentityRecord struct{}

func (_ FfiDestroyerTypeIdsUsersWithIdentityRecord) Destroy(value IdsUsersWithIdentityRecord) {
	value.Destroy()
}

type SharedAlbumInfo struct {
	Albumguid string
	Name      *string
	Fullname  *string
	Email     *string
}

func (r *SharedAlbumInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Albumguid)
	FfiDestroyerOptionalString{}.Destroy(r.Name)
	FfiDestroyerOptionalString{}.Destroy(r.Fullname)
	FfiDestroyerOptionalString{}.Destroy(r.Email)
}

type FfiConverterTypeSharedAlbumInfo struct{}

var FfiConverterTypeSharedAlbumInfoINSTANCE = FfiConverterTypeSharedAlbumInfo{}

func (c FfiConverterTypeSharedAlbumInfo) Lift(rb RustBufferI) SharedAlbumInfo {
	return LiftFromRustBuffer[SharedAlbumInfo](c, rb)
}

func (c FfiConverterTypeSharedAlbumInfo) Read(reader io.Reader) SharedAlbumInfo {
	return SharedAlbumInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeSharedAlbumInfo) Lower(value SharedAlbumInfo) RustBuffer {
	return LowerIntoRustBuffer[SharedAlbumInfo](c, value)
}

func (c FfiConverterTypeSharedAlbumInfo) Write(writer io.Writer, value SharedAlbumInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Albumguid)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Name)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Fullname)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Email)
}

type FfiDestroyerTypeSharedAlbumInfo struct{}

func (_ FfiDestroyerTypeSharedAlbumInfo) Destroy(value SharedAlbumInfo) {
	value.Destroy()
}

type SharedAssetInfo struct {
	Assetguid   string
	Filename    string
	DateCreated string
	MediaType   string
	Width       string
	Height      string
	Size        string
}

func (r *SharedAssetInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Assetguid)
	FfiDestroyerString{}.Destroy(r.Filename)
	FfiDestroyerString{}.Destroy(r.DateCreated)
	FfiDestroyerString{}.Destroy(r.MediaType)
	FfiDestroyerString{}.Destroy(r.Width)
	FfiDestroyerString{}.Destroy(r.Height)
	FfiDestroyerString{}.Destroy(r.Size)
}

type FfiConverterTypeSharedAssetInfo struct{}

var FfiConverterTypeSharedAssetInfoINSTANCE = FfiConverterTypeSharedAssetInfo{}

func (c FfiConverterTypeSharedAssetInfo) Lift(rb RustBufferI) SharedAssetInfo {
	return LiftFromRustBuffer[SharedAssetInfo](c, rb)
}

func (c FfiConverterTypeSharedAssetInfo) Read(reader io.Reader) SharedAssetInfo {
	return SharedAssetInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeSharedAssetInfo) Lower(value SharedAssetInfo) RustBuffer {
	return LowerIntoRustBuffer[SharedAssetInfo](c, value)
}

func (c FfiConverterTypeSharedAssetInfo) Write(writer io.Writer, value SharedAssetInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Assetguid)
	FfiConverterStringINSTANCE.Write(writer, value.Filename)
	FfiConverterStringINSTANCE.Write(writer, value.DateCreated)
	FfiConverterStringINSTANCE.Write(writer, value.MediaType)
	FfiConverterStringINSTANCE.Write(writer, value.Width)
	FfiConverterStringINSTANCE.Write(writer, value.Height)
	FfiConverterStringINSTANCE.Write(writer, value.Size)
}

type FfiDestroyerTypeSharedAssetInfo struct{}

func (_ FfiDestroyerTypeSharedAssetInfo) Destroy(value SharedAssetInfo) {
	value.Destroy()
}

type WrappedAttachment struct {
	MimeType           string
	Filename           string
	UtiType            string
	Size               uint64
	IsInline           bool
	InlineData         *[]byte
	Iris               bool
	MmcsDescriptorJson *string
}

func (r *WrappedAttachment) Destroy() {
	FfiDestroyerString{}.Destroy(r.MimeType)
	FfiDestroyerString{}.Destroy(r.Filename)
	FfiDestroyerString{}.Destroy(r.UtiType)
	FfiDestroyerUint64{}.Destroy(r.Size)
	FfiDestroyerBool{}.Destroy(r.IsInline)
	FfiDestroyerOptionalBytes{}.Destroy(r.InlineData)
	FfiDestroyerBool{}.Destroy(r.Iris)
	FfiDestroyerOptionalString{}.Destroy(r.MmcsDescriptorJson)
}

type FfiConverterTypeWrappedAttachment struct{}

var FfiConverterTypeWrappedAttachmentINSTANCE = FfiConverterTypeWrappedAttachment{}

func (c FfiConverterTypeWrappedAttachment) Lift(rb RustBufferI) WrappedAttachment {
	return LiftFromRustBuffer[WrappedAttachment](c, rb)
}

func (c FfiConverterTypeWrappedAttachment) Read(reader io.Reader) WrappedAttachment {
	return WrappedAttachment{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedAttachment) Lower(value WrappedAttachment) RustBuffer {
	return LowerIntoRustBuffer[WrappedAttachment](c, value)
}

func (c FfiConverterTypeWrappedAttachment) Write(writer io.Writer, value WrappedAttachment) {
	FfiConverterStringINSTANCE.Write(writer, value.MimeType)
	FfiConverterStringINSTANCE.Write(writer, value.Filename)
	FfiConverterStringINSTANCE.Write(writer, value.UtiType)
	FfiConverterUint64INSTANCE.Write(writer, value.Size)
	FfiConverterBoolINSTANCE.Write(writer, value.IsInline)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.InlineData)
	FfiConverterBoolINSTANCE.Write(writer, value.Iris)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MmcsDescriptorJson)
}

type FfiDestroyerTypeWrappedAttachment struct{}

func (_ FfiDestroyerTypeWrappedAttachment) Destroy(value WrappedAttachment) {
	value.Destroy()
}

type WrappedCloudAttachmentInfo struct {
	Guid           string
	MimeType       *string
	UtiType        *string
	Filename       *string
	FileSize       int64
	RecordName     string
	HideAttachment bool
	HasAvid        bool
	FordKey        *[]byte
	AvidFordKey    *[]byte
}

func (r *WrappedCloudAttachmentInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Guid)
	FfiDestroyerOptionalString{}.Destroy(r.MimeType)
	FfiDestroyerOptionalString{}.Destroy(r.UtiType)
	FfiDestroyerOptionalString{}.Destroy(r.Filename)
	FfiDestroyerInt64{}.Destroy(r.FileSize)
	FfiDestroyerString{}.Destroy(r.RecordName)
	FfiDestroyerBool{}.Destroy(r.HideAttachment)
	FfiDestroyerBool{}.Destroy(r.HasAvid)
	FfiDestroyerOptionalBytes{}.Destroy(r.FordKey)
	FfiDestroyerOptionalBytes{}.Destroy(r.AvidFordKey)
}

type FfiConverterTypeWrappedCloudAttachmentInfo struct{}

var FfiConverterTypeWrappedCloudAttachmentInfoINSTANCE = FfiConverterTypeWrappedCloudAttachmentInfo{}

func (c FfiConverterTypeWrappedCloudAttachmentInfo) Lift(rb RustBufferI) WrappedCloudAttachmentInfo {
	return LiftFromRustBuffer[WrappedCloudAttachmentInfo](c, rb)
}

func (c FfiConverterTypeWrappedCloudAttachmentInfo) Read(reader io.Reader) WrappedCloudAttachmentInfo {
	return WrappedCloudAttachmentInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudAttachmentInfo) Lower(value WrappedCloudAttachmentInfo) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudAttachmentInfo](c, value)
}

func (c FfiConverterTypeWrappedCloudAttachmentInfo) Write(writer io.Writer, value WrappedCloudAttachmentInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Guid)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MimeType)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UtiType)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Filename)
	FfiConverterInt64INSTANCE.Write(writer, value.FileSize)
	FfiConverterStringINSTANCE.Write(writer, value.RecordName)
	FfiConverterBoolINSTANCE.Write(writer, value.HideAttachment)
	FfiConverterBoolINSTANCE.Write(writer, value.HasAvid)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.FordKey)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.AvidFordKey)
}

type FfiDestroyerTypeWrappedCloudAttachmentInfo struct{}

func (_ FfiDestroyerTypeWrappedCloudAttachmentInfo) Destroy(value WrappedCloudAttachmentInfo) {
	value.Destroy()
}

type WrappedCloudSyncAttachmentsPage struct {
	ContinuationToken *string
	Status            int32
	Done              bool
	Attachments       []WrappedCloudAttachmentInfo
}

func (r *WrappedCloudSyncAttachmentsPage) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.ContinuationToken)
	FfiDestroyerInt32{}.Destroy(r.Status)
	FfiDestroyerBool{}.Destroy(r.Done)
	FfiDestroyerSequenceTypeWrappedCloudAttachmentInfo{}.Destroy(r.Attachments)
}

type FfiConverterTypeWrappedCloudSyncAttachmentsPage struct{}

var FfiConverterTypeWrappedCloudSyncAttachmentsPageINSTANCE = FfiConverterTypeWrappedCloudSyncAttachmentsPage{}

func (c FfiConverterTypeWrappedCloudSyncAttachmentsPage) Lift(rb RustBufferI) WrappedCloudSyncAttachmentsPage {
	return LiftFromRustBuffer[WrappedCloudSyncAttachmentsPage](c, rb)
}

func (c FfiConverterTypeWrappedCloudSyncAttachmentsPage) Read(reader io.Reader) WrappedCloudSyncAttachmentsPage {
	return WrappedCloudSyncAttachmentsPage{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterInt32INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceTypeWrappedCloudAttachmentInfoINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudSyncAttachmentsPage) Lower(value WrappedCloudSyncAttachmentsPage) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudSyncAttachmentsPage](c, value)
}

func (c FfiConverterTypeWrappedCloudSyncAttachmentsPage) Write(writer io.Writer, value WrappedCloudSyncAttachmentsPage) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ContinuationToken)
	FfiConverterInt32INSTANCE.Write(writer, value.Status)
	FfiConverterBoolINSTANCE.Write(writer, value.Done)
	FfiConverterSequenceTypeWrappedCloudAttachmentInfoINSTANCE.Write(writer, value.Attachments)
}

type FfiDestroyerTypeWrappedCloudSyncAttachmentsPage struct{}

func (_ FfiDestroyerTypeWrappedCloudSyncAttachmentsPage) Destroy(value WrappedCloudSyncAttachmentsPage) {
	value.Destroy()
}

type WrappedCloudSyncChat struct {
	RecordName         string
	CloudChatId        string
	GroupId            string
	Style              int64
	Service            string
	DisplayName        *string
	Participants       []string
	Deleted            bool
	UpdatedTimestampMs uint64
	GroupPhotoGuid     *string
	IsFiltered         int64
}

func (r *WrappedCloudSyncChat) Destroy() {
	FfiDestroyerString{}.Destroy(r.RecordName)
	FfiDestroyerString{}.Destroy(r.CloudChatId)
	FfiDestroyerString{}.Destroy(r.GroupId)
	FfiDestroyerInt64{}.Destroy(r.Style)
	FfiDestroyerString{}.Destroy(r.Service)
	FfiDestroyerOptionalString{}.Destroy(r.DisplayName)
	FfiDestroyerSequenceString{}.Destroy(r.Participants)
	FfiDestroyerBool{}.Destroy(r.Deleted)
	FfiDestroyerUint64{}.Destroy(r.UpdatedTimestampMs)
	FfiDestroyerOptionalString{}.Destroy(r.GroupPhotoGuid)
	FfiDestroyerInt64{}.Destroy(r.IsFiltered)
}

type FfiConverterTypeWrappedCloudSyncChat struct{}

var FfiConverterTypeWrappedCloudSyncChatINSTANCE = FfiConverterTypeWrappedCloudSyncChat{}

func (c FfiConverterTypeWrappedCloudSyncChat) Lift(rb RustBufferI) WrappedCloudSyncChat {
	return LiftFromRustBuffer[WrappedCloudSyncChat](c, rb)
}

func (c FfiConverterTypeWrappedCloudSyncChat) Read(reader io.Reader) WrappedCloudSyncChat {
	return WrappedCloudSyncChat{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudSyncChat) Lower(value WrappedCloudSyncChat) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudSyncChat](c, value)
}

func (c FfiConverterTypeWrappedCloudSyncChat) Write(writer io.Writer, value WrappedCloudSyncChat) {
	FfiConverterStringINSTANCE.Write(writer, value.RecordName)
	FfiConverterStringINSTANCE.Write(writer, value.CloudChatId)
	FfiConverterStringINSTANCE.Write(writer, value.GroupId)
	FfiConverterInt64INSTANCE.Write(writer, value.Style)
	FfiConverterStringINSTANCE.Write(writer, value.Service)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.DisplayName)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Participants)
	FfiConverterBoolINSTANCE.Write(writer, value.Deleted)
	FfiConverterUint64INSTANCE.Write(writer, value.UpdatedTimestampMs)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.GroupPhotoGuid)
	FfiConverterInt64INSTANCE.Write(writer, value.IsFiltered)
}

type FfiDestroyerTypeWrappedCloudSyncChat struct{}

func (_ FfiDestroyerTypeWrappedCloudSyncChat) Destroy(value WrappedCloudSyncChat) {
	value.Destroy()
}

type WrappedCloudSyncChatsPage struct {
	ContinuationToken *string
	Status            int32
	Done              bool
	Chats             []WrappedCloudSyncChat
}

func (r *WrappedCloudSyncChatsPage) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.ContinuationToken)
	FfiDestroyerInt32{}.Destroy(r.Status)
	FfiDestroyerBool{}.Destroy(r.Done)
	FfiDestroyerSequenceTypeWrappedCloudSyncChat{}.Destroy(r.Chats)
}

type FfiConverterTypeWrappedCloudSyncChatsPage struct{}

var FfiConverterTypeWrappedCloudSyncChatsPageINSTANCE = FfiConverterTypeWrappedCloudSyncChatsPage{}

func (c FfiConverterTypeWrappedCloudSyncChatsPage) Lift(rb RustBufferI) WrappedCloudSyncChatsPage {
	return LiftFromRustBuffer[WrappedCloudSyncChatsPage](c, rb)
}

func (c FfiConverterTypeWrappedCloudSyncChatsPage) Read(reader io.Reader) WrappedCloudSyncChatsPage {
	return WrappedCloudSyncChatsPage{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterInt32INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceTypeWrappedCloudSyncChatINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudSyncChatsPage) Lower(value WrappedCloudSyncChatsPage) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudSyncChatsPage](c, value)
}

func (c FfiConverterTypeWrappedCloudSyncChatsPage) Write(writer io.Writer, value WrappedCloudSyncChatsPage) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ContinuationToken)
	FfiConverterInt32INSTANCE.Write(writer, value.Status)
	FfiConverterBoolINSTANCE.Write(writer, value.Done)
	FfiConverterSequenceTypeWrappedCloudSyncChatINSTANCE.Write(writer, value.Chats)
}

type FfiDestroyerTypeWrappedCloudSyncChatsPage struct{}

func (_ FfiDestroyerTypeWrappedCloudSyncChatsPage) Destroy(value WrappedCloudSyncChatsPage) {
	value.Destroy()
}

type WrappedCloudSyncMessage struct {
	RecordName        string
	Guid              string
	CloudChatId       string
	Sender            string
	IsFromMe          bool
	Text              *string
	Subject           *string
	Service           string
	TimestampMs       int64
	Deleted           bool
	TapbackType       *uint32
	TapbackTargetGuid *string
	TapbackEmoji      *string
	AttachmentGuids   []string
	DateReadMs        int64
	MsgType           int64
	HasBody           bool
}

func (r *WrappedCloudSyncMessage) Destroy() {
	FfiDestroyerString{}.Destroy(r.RecordName)
	FfiDestroyerString{}.Destroy(r.Guid)
	FfiDestroyerString{}.Destroy(r.CloudChatId)
	FfiDestroyerString{}.Destroy(r.Sender)
	FfiDestroyerBool{}.Destroy(r.IsFromMe)
	FfiDestroyerOptionalString{}.Destroy(r.Text)
	FfiDestroyerOptionalString{}.Destroy(r.Subject)
	FfiDestroyerString{}.Destroy(r.Service)
	FfiDestroyerInt64{}.Destroy(r.TimestampMs)
	FfiDestroyerBool{}.Destroy(r.Deleted)
	FfiDestroyerOptionalUint32{}.Destroy(r.TapbackType)
	FfiDestroyerOptionalString{}.Destroy(r.TapbackTargetGuid)
	FfiDestroyerOptionalString{}.Destroy(r.TapbackEmoji)
	FfiDestroyerSequenceString{}.Destroy(r.AttachmentGuids)
	FfiDestroyerInt64{}.Destroy(r.DateReadMs)
	FfiDestroyerInt64{}.Destroy(r.MsgType)
	FfiDestroyerBool{}.Destroy(r.HasBody)
}

type FfiConverterTypeWrappedCloudSyncMessage struct{}

var FfiConverterTypeWrappedCloudSyncMessageINSTANCE = FfiConverterTypeWrappedCloudSyncMessage{}

func (c FfiConverterTypeWrappedCloudSyncMessage) Lift(rb RustBufferI) WrappedCloudSyncMessage {
	return LiftFromRustBuffer[WrappedCloudSyncMessage](c, rb)
}

func (c FfiConverterTypeWrappedCloudSyncMessage) Read(reader io.Reader) WrappedCloudSyncMessage {
	return WrappedCloudSyncMessage{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudSyncMessage) Lower(value WrappedCloudSyncMessage) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudSyncMessage](c, value)
}

func (c FfiConverterTypeWrappedCloudSyncMessage) Write(writer io.Writer, value WrappedCloudSyncMessage) {
	FfiConverterStringINSTANCE.Write(writer, value.RecordName)
	FfiConverterStringINSTANCE.Write(writer, value.Guid)
	FfiConverterStringINSTANCE.Write(writer, value.CloudChatId)
	FfiConverterStringINSTANCE.Write(writer, value.Sender)
	FfiConverterBoolINSTANCE.Write(writer, value.IsFromMe)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Text)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Subject)
	FfiConverterStringINSTANCE.Write(writer, value.Service)
	FfiConverterInt64INSTANCE.Write(writer, value.TimestampMs)
	FfiConverterBoolINSTANCE.Write(writer, value.Deleted)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.TapbackType)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TapbackTargetGuid)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TapbackEmoji)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.AttachmentGuids)
	FfiConverterInt64INSTANCE.Write(writer, value.DateReadMs)
	FfiConverterInt64INSTANCE.Write(writer, value.MsgType)
	FfiConverterBoolINSTANCE.Write(writer, value.HasBody)
}

type FfiDestroyerTypeWrappedCloudSyncMessage struct{}

func (_ FfiDestroyerTypeWrappedCloudSyncMessage) Destroy(value WrappedCloudSyncMessage) {
	value.Destroy()
}

type WrappedCloudSyncMessagesPage struct {
	ContinuationToken *string
	Status            int32
	Done              bool
	Messages          []WrappedCloudSyncMessage
}

func (r *WrappedCloudSyncMessagesPage) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.ContinuationToken)
	FfiDestroyerInt32{}.Destroy(r.Status)
	FfiDestroyerBool{}.Destroy(r.Done)
	FfiDestroyerSequenceTypeWrappedCloudSyncMessage{}.Destroy(r.Messages)
}

type FfiConverterTypeWrappedCloudSyncMessagesPage struct{}

var FfiConverterTypeWrappedCloudSyncMessagesPageINSTANCE = FfiConverterTypeWrappedCloudSyncMessagesPage{}

func (c FfiConverterTypeWrappedCloudSyncMessagesPage) Lift(rb RustBufferI) WrappedCloudSyncMessagesPage {
	return LiftFromRustBuffer[WrappedCloudSyncMessagesPage](c, rb)
}

func (c FfiConverterTypeWrappedCloudSyncMessagesPage) Read(reader io.Reader) WrappedCloudSyncMessagesPage {
	return WrappedCloudSyncMessagesPage{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterInt32INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceTypeWrappedCloudSyncMessageINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedCloudSyncMessagesPage) Lower(value WrappedCloudSyncMessagesPage) RustBuffer {
	return LowerIntoRustBuffer[WrappedCloudSyncMessagesPage](c, value)
}

func (c FfiConverterTypeWrappedCloudSyncMessagesPage) Write(writer io.Writer, value WrappedCloudSyncMessagesPage) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ContinuationToken)
	FfiConverterInt32INSTANCE.Write(writer, value.Status)
	FfiConverterBoolINSTANCE.Write(writer, value.Done)
	FfiConverterSequenceTypeWrappedCloudSyncMessageINSTANCE.Write(writer, value.Messages)
}

type FfiDestroyerTypeWrappedCloudSyncMessagesPage struct{}

func (_ FfiDestroyerTypeWrappedCloudSyncMessagesPage) Destroy(value WrappedCloudSyncMessagesPage) {
	value.Destroy()
}

type WrappedConversation struct {
	Participants []string
	GroupName    *string
	SenderGuid   *string
	IsSms        bool
}

func (r *WrappedConversation) Destroy() {
	FfiDestroyerSequenceString{}.Destroy(r.Participants)
	FfiDestroyerOptionalString{}.Destroy(r.GroupName)
	FfiDestroyerOptionalString{}.Destroy(r.SenderGuid)
	FfiDestroyerBool{}.Destroy(r.IsSms)
}

type FfiConverterTypeWrappedConversation struct{}

var FfiConverterTypeWrappedConversationINSTANCE = FfiConverterTypeWrappedConversation{}

func (c FfiConverterTypeWrappedConversation) Lift(rb RustBufferI) WrappedConversation {
	return LiftFromRustBuffer[WrappedConversation](c, rb)
}

func (c FfiConverterTypeWrappedConversation) Read(reader io.Reader) WrappedConversation {
	return WrappedConversation{
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedConversation) Lower(value WrappedConversation) RustBuffer {
	return LowerIntoRustBuffer[WrappedConversation](c, value)
}

func (c FfiConverterTypeWrappedConversation) Write(writer io.Writer, value WrappedConversation) {
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Participants)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.GroupName)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.SenderGuid)
	FfiConverterBoolINSTANCE.Write(writer, value.IsSms)
}

type FfiDestroyerTypeWrappedConversation struct{}

func (_ FfiDestroyerTypeWrappedConversation) Destroy(value WrappedConversation) {
	value.Destroy()
}

type WrappedLetMeInRequest struct {
	DelegationUuid string
	Pseud          string
	Requestor      string
	Nickname       *string
	Usage          *string
}

func (r *WrappedLetMeInRequest) Destroy() {
	FfiDestroyerString{}.Destroy(r.DelegationUuid)
	FfiDestroyerString{}.Destroy(r.Pseud)
	FfiDestroyerString{}.Destroy(r.Requestor)
	FfiDestroyerOptionalString{}.Destroy(r.Nickname)
	FfiDestroyerOptionalString{}.Destroy(r.Usage)
}

type FfiConverterTypeWrappedLetMeInRequest struct{}

var FfiConverterTypeWrappedLetMeInRequestINSTANCE = FfiConverterTypeWrappedLetMeInRequest{}

func (c FfiConverterTypeWrappedLetMeInRequest) Lift(rb RustBufferI) WrappedLetMeInRequest {
	return LiftFromRustBuffer[WrappedLetMeInRequest](c, rb)
}

func (c FfiConverterTypeWrappedLetMeInRequest) Read(reader io.Reader) WrappedLetMeInRequest {
	return WrappedLetMeInRequest{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedLetMeInRequest) Lower(value WrappedLetMeInRequest) RustBuffer {
	return LowerIntoRustBuffer[WrappedLetMeInRequest](c, value)
}

func (c FfiConverterTypeWrappedLetMeInRequest) Write(writer io.Writer, value WrappedLetMeInRequest) {
	FfiConverterStringINSTANCE.Write(writer, value.DelegationUuid)
	FfiConverterStringINSTANCE.Write(writer, value.Pseud)
	FfiConverterStringINSTANCE.Write(writer, value.Requestor)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Nickname)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Usage)
}

type FfiDestroyerTypeWrappedLetMeInRequest struct{}

func (_ FfiDestroyerTypeWrappedLetMeInRequest) Destroy(value WrappedLetMeInRequest) {
	value.Destroy()
}

type WrappedMessage struct {
	Uuid                          string
	Sender                        *string
	Text                          *string
	Subject                       *string
	Participants                  []string
	GroupName                     *string
	TimestampMs                   uint64
	IsSms                         bool
	IsTapback                     bool
	TapbackType                   *uint32
	TapbackTargetUuid             *string
	TapbackTargetPart             *uint64
	TapbackEmoji                  *string
	TapbackRemove                 bool
	IsEdit                        bool
	EditTargetUuid                *string
	EditPart                      *uint64
	EditNewText                   *string
	IsUnsend                      bool
	UnsendTargetUuid              *string
	UnsendEditPart                *uint64
	IsRename                      bool
	NewChatName                   *string
	IsParticipantChange           bool
	NewParticipants               []string
	Attachments                   []WrappedAttachment
	ReplyGuid                     *string
	ReplyPart                     *string
	IsTyping                      bool
	TypingAppBundleId             *string
	TypingAppIcon                 *[]byte
	IsReadReceipt                 bool
	IsDelivered                   bool
	IsError                       bool
	ErrorForUuid                  *string
	ErrorStatus                   *uint64
	ErrorStatusStr                *string
	IsPeerCacheInvalidate         bool
	SendDelivered                 bool
	SenderGuid                    *string
	IsMoveToRecycleBin            bool
	IsPermanentDelete             bool
	IsRecoverChat                 bool
	DeleteChatParticipants        []string
	DeleteChatGroupId             *string
	DeleteChatGuid                *string
	DeleteMessageUuids            []string
	IsStoredMessage               bool
	IsIconChange                  bool
	GroupPhotoCleared             bool
	IconChangePhotoData           *[]byte
	Html                          *string
	IsVoice                       bool
	Effect                        *string
	ScheduledMs                   *uint64
	IsSmsActivation               *bool
	IsSmsConfirmSent              *bool
	IsMarkUnread                  bool
	IsMessageReadOnDevice         bool
	IsUnschedule                  bool
	IsUpdateExtension             bool
	UpdateExtensionForUuid        *string
	IsUpdateProfileSharing        bool
	UpdateProfileSharingDismissed []string
	UpdateProfileSharingAll       []string
	UpdateProfileSharingVersion   *uint64
	IsUpdateProfile               bool
	UpdateProfileShareContacts    *bool
	IsNotifyAnyways               bool
	IsSetTranscriptBackground     bool
	TranscriptBackgroundRemove    *bool
	TranscriptBackgroundChatId    *string
	TranscriptBackgroundObjectId  *string
	TranscriptBackgroundUrl       *string
	TranscriptBackgroundFileSize  *uint64
	StickerData                   *[]byte
	StickerMime                   *string
	IsShareProfile                bool
	ShareProfileRecordKey         *string
	ShareProfileDecryptionKey     *[]byte
	ShareProfileHasPoster         bool
	ShareProfileDisplayName       *string
	ShareProfileFirstName         *string
	ShareProfileLastName          *string
	ShareProfileAvatar            *[]byte
}

func (r *WrappedMessage) Destroy() {
	FfiDestroyerString{}.Destroy(r.Uuid)
	FfiDestroyerOptionalString{}.Destroy(r.Sender)
	FfiDestroyerOptionalString{}.Destroy(r.Text)
	FfiDestroyerOptionalString{}.Destroy(r.Subject)
	FfiDestroyerSequenceString{}.Destroy(r.Participants)
	FfiDestroyerOptionalString{}.Destroy(r.GroupName)
	FfiDestroyerUint64{}.Destroy(r.TimestampMs)
	FfiDestroyerBool{}.Destroy(r.IsSms)
	FfiDestroyerBool{}.Destroy(r.IsTapback)
	FfiDestroyerOptionalUint32{}.Destroy(r.TapbackType)
	FfiDestroyerOptionalString{}.Destroy(r.TapbackTargetUuid)
	FfiDestroyerOptionalUint64{}.Destroy(r.TapbackTargetPart)
	FfiDestroyerOptionalString{}.Destroy(r.TapbackEmoji)
	FfiDestroyerBool{}.Destroy(r.TapbackRemove)
	FfiDestroyerBool{}.Destroy(r.IsEdit)
	FfiDestroyerOptionalString{}.Destroy(r.EditTargetUuid)
	FfiDestroyerOptionalUint64{}.Destroy(r.EditPart)
	FfiDestroyerOptionalString{}.Destroy(r.EditNewText)
	FfiDestroyerBool{}.Destroy(r.IsUnsend)
	FfiDestroyerOptionalString{}.Destroy(r.UnsendTargetUuid)
	FfiDestroyerOptionalUint64{}.Destroy(r.UnsendEditPart)
	FfiDestroyerBool{}.Destroy(r.IsRename)
	FfiDestroyerOptionalString{}.Destroy(r.NewChatName)
	FfiDestroyerBool{}.Destroy(r.IsParticipantChange)
	FfiDestroyerSequenceString{}.Destroy(r.NewParticipants)
	FfiDestroyerSequenceTypeWrappedAttachment{}.Destroy(r.Attachments)
	FfiDestroyerOptionalString{}.Destroy(r.ReplyGuid)
	FfiDestroyerOptionalString{}.Destroy(r.ReplyPart)
	FfiDestroyerBool{}.Destroy(r.IsTyping)
	FfiDestroyerOptionalString{}.Destroy(r.TypingAppBundleId)
	FfiDestroyerOptionalBytes{}.Destroy(r.TypingAppIcon)
	FfiDestroyerBool{}.Destroy(r.IsReadReceipt)
	FfiDestroyerBool{}.Destroy(r.IsDelivered)
	FfiDestroyerBool{}.Destroy(r.IsError)
	FfiDestroyerOptionalString{}.Destroy(r.ErrorForUuid)
	FfiDestroyerOptionalUint64{}.Destroy(r.ErrorStatus)
	FfiDestroyerOptionalString{}.Destroy(r.ErrorStatusStr)
	FfiDestroyerBool{}.Destroy(r.IsPeerCacheInvalidate)
	FfiDestroyerBool{}.Destroy(r.SendDelivered)
	FfiDestroyerOptionalString{}.Destroy(r.SenderGuid)
	FfiDestroyerBool{}.Destroy(r.IsMoveToRecycleBin)
	FfiDestroyerBool{}.Destroy(r.IsPermanentDelete)
	FfiDestroyerBool{}.Destroy(r.IsRecoverChat)
	FfiDestroyerSequenceString{}.Destroy(r.DeleteChatParticipants)
	FfiDestroyerOptionalString{}.Destroy(r.DeleteChatGroupId)
	FfiDestroyerOptionalString{}.Destroy(r.DeleteChatGuid)
	FfiDestroyerSequenceString{}.Destroy(r.DeleteMessageUuids)
	FfiDestroyerBool{}.Destroy(r.IsStoredMessage)
	FfiDestroyerBool{}.Destroy(r.IsIconChange)
	FfiDestroyerBool{}.Destroy(r.GroupPhotoCleared)
	FfiDestroyerOptionalBytes{}.Destroy(r.IconChangePhotoData)
	FfiDestroyerOptionalString{}.Destroy(r.Html)
	FfiDestroyerBool{}.Destroy(r.IsVoice)
	FfiDestroyerOptionalString{}.Destroy(r.Effect)
	FfiDestroyerOptionalUint64{}.Destroy(r.ScheduledMs)
	FfiDestroyerOptionalBool{}.Destroy(r.IsSmsActivation)
	FfiDestroyerOptionalBool{}.Destroy(r.IsSmsConfirmSent)
	FfiDestroyerBool{}.Destroy(r.IsMarkUnread)
	FfiDestroyerBool{}.Destroy(r.IsMessageReadOnDevice)
	FfiDestroyerBool{}.Destroy(r.IsUnschedule)
	FfiDestroyerBool{}.Destroy(r.IsUpdateExtension)
	FfiDestroyerOptionalString{}.Destroy(r.UpdateExtensionForUuid)
	FfiDestroyerBool{}.Destroy(r.IsUpdateProfileSharing)
	FfiDestroyerSequenceString{}.Destroy(r.UpdateProfileSharingDismissed)
	FfiDestroyerSequenceString{}.Destroy(r.UpdateProfileSharingAll)
	FfiDestroyerOptionalUint64{}.Destroy(r.UpdateProfileSharingVersion)
	FfiDestroyerBool{}.Destroy(r.IsUpdateProfile)
	FfiDestroyerOptionalBool{}.Destroy(r.UpdateProfileShareContacts)
	FfiDestroyerBool{}.Destroy(r.IsNotifyAnyways)
	FfiDestroyerBool{}.Destroy(r.IsSetTranscriptBackground)
	FfiDestroyerOptionalBool{}.Destroy(r.TranscriptBackgroundRemove)
	FfiDestroyerOptionalString{}.Destroy(r.TranscriptBackgroundChatId)
	FfiDestroyerOptionalString{}.Destroy(r.TranscriptBackgroundObjectId)
	FfiDestroyerOptionalString{}.Destroy(r.TranscriptBackgroundUrl)
	FfiDestroyerOptionalUint64{}.Destroy(r.TranscriptBackgroundFileSize)
	FfiDestroyerOptionalBytes{}.Destroy(r.StickerData)
	FfiDestroyerOptionalString{}.Destroy(r.StickerMime)
	FfiDestroyerBool{}.Destroy(r.IsShareProfile)
	FfiDestroyerOptionalString{}.Destroy(r.ShareProfileRecordKey)
	FfiDestroyerOptionalBytes{}.Destroy(r.ShareProfileDecryptionKey)
	FfiDestroyerBool{}.Destroy(r.ShareProfileHasPoster)
	FfiDestroyerOptionalString{}.Destroy(r.ShareProfileDisplayName)
	FfiDestroyerOptionalString{}.Destroy(r.ShareProfileFirstName)
	FfiDestroyerOptionalString{}.Destroy(r.ShareProfileLastName)
	FfiDestroyerOptionalBytes{}.Destroy(r.ShareProfileAvatar)
}

type FfiConverterTypeWrappedMessage struct{}

var FfiConverterTypeWrappedMessageINSTANCE = FfiConverterTypeWrappedMessage{}

func (c FfiConverterTypeWrappedMessage) Lift(rb RustBufferI) WrappedMessage {
	return LiftFromRustBuffer[WrappedMessage](c, rb)
}

func (c FfiConverterTypeWrappedMessage) Read(reader io.Reader) WrappedMessage {
	return WrappedMessage{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceTypeWrappedAttachmentINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedMessage) Lower(value WrappedMessage) RustBuffer {
	return LowerIntoRustBuffer[WrappedMessage](c, value)
}

func (c FfiConverterTypeWrappedMessage) Write(writer io.Writer, value WrappedMessage) {
	FfiConverterStringINSTANCE.Write(writer, value.Uuid)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Sender)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Text)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Subject)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.Participants)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.GroupName)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampMs)
	FfiConverterBoolINSTANCE.Write(writer, value.IsSms)
	FfiConverterBoolINSTANCE.Write(writer, value.IsTapback)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.TapbackType)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TapbackTargetUuid)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.TapbackTargetPart)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TapbackEmoji)
	FfiConverterBoolINSTANCE.Write(writer, value.TapbackRemove)
	FfiConverterBoolINSTANCE.Write(writer, value.IsEdit)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.EditTargetUuid)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.EditPart)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.EditNewText)
	FfiConverterBoolINSTANCE.Write(writer, value.IsUnsend)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UnsendTargetUuid)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.UnsendEditPart)
	FfiConverterBoolINSTANCE.Write(writer, value.IsRename)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.NewChatName)
	FfiConverterBoolINSTANCE.Write(writer, value.IsParticipantChange)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.NewParticipants)
	FfiConverterSequenceTypeWrappedAttachmentINSTANCE.Write(writer, value.Attachments)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ReplyGuid)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ReplyPart)
	FfiConverterBoolINSTANCE.Write(writer, value.IsTyping)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TypingAppBundleId)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.TypingAppIcon)
	FfiConverterBoolINSTANCE.Write(writer, value.IsReadReceipt)
	FfiConverterBoolINSTANCE.Write(writer, value.IsDelivered)
	FfiConverterBoolINSTANCE.Write(writer, value.IsError)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ErrorForUuid)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.ErrorStatus)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ErrorStatusStr)
	FfiConverterBoolINSTANCE.Write(writer, value.IsPeerCacheInvalidate)
	FfiConverterBoolINSTANCE.Write(writer, value.SendDelivered)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.SenderGuid)
	FfiConverterBoolINSTANCE.Write(writer, value.IsMoveToRecycleBin)
	FfiConverterBoolINSTANCE.Write(writer, value.IsPermanentDelete)
	FfiConverterBoolINSTANCE.Write(writer, value.IsRecoverChat)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.DeleteChatParticipants)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.DeleteChatGroupId)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.DeleteChatGuid)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.DeleteMessageUuids)
	FfiConverterBoolINSTANCE.Write(writer, value.IsStoredMessage)
	FfiConverterBoolINSTANCE.Write(writer, value.IsIconChange)
	FfiConverterBoolINSTANCE.Write(writer, value.GroupPhotoCleared)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.IconChangePhotoData)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Html)
	FfiConverterBoolINSTANCE.Write(writer, value.IsVoice)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Effect)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.ScheduledMs)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.IsSmsActivation)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.IsSmsConfirmSent)
	FfiConverterBoolINSTANCE.Write(writer, value.IsMarkUnread)
	FfiConverterBoolINSTANCE.Write(writer, value.IsMessageReadOnDevice)
	FfiConverterBoolINSTANCE.Write(writer, value.IsUnschedule)
	FfiConverterBoolINSTANCE.Write(writer, value.IsUpdateExtension)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UpdateExtensionForUuid)
	FfiConverterBoolINSTANCE.Write(writer, value.IsUpdateProfileSharing)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.UpdateProfileSharingDismissed)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.UpdateProfileSharingAll)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.UpdateProfileSharingVersion)
	FfiConverterBoolINSTANCE.Write(writer, value.IsUpdateProfile)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.UpdateProfileShareContacts)
	FfiConverterBoolINSTANCE.Write(writer, value.IsNotifyAnyways)
	FfiConverterBoolINSTANCE.Write(writer, value.IsSetTranscriptBackground)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.TranscriptBackgroundRemove)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TranscriptBackgroundChatId)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TranscriptBackgroundObjectId)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.TranscriptBackgroundUrl)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.TranscriptBackgroundFileSize)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.StickerData)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.StickerMime)
	FfiConverterBoolINSTANCE.Write(writer, value.IsShareProfile)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ShareProfileRecordKey)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.ShareProfileDecryptionKey)
	FfiConverterBoolINSTANCE.Write(writer, value.ShareProfileHasPoster)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ShareProfileDisplayName)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ShareProfileFirstName)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ShareProfileLastName)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.ShareProfileAvatar)
}

type FfiDestroyerTypeWrappedMessage struct{}

func (_ FfiDestroyerTypeWrappedMessage) Destroy(value WrappedMessage) {
	value.Destroy()
}

type WrappedPasswordEntryRef struct {
	Id    string
	Group *string
}

func (r *WrappedPasswordEntryRef) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerOptionalString{}.Destroy(r.Group)
}

type FfiConverterTypeWrappedPasswordEntryRef struct{}

var FfiConverterTypeWrappedPasswordEntryRefINSTANCE = FfiConverterTypeWrappedPasswordEntryRef{}

func (c FfiConverterTypeWrappedPasswordEntryRef) Lift(rb RustBufferI) WrappedPasswordEntryRef {
	return LiftFromRustBuffer[WrappedPasswordEntryRef](c, rb)
}

func (c FfiConverterTypeWrappedPasswordEntryRef) Read(reader io.Reader) WrappedPasswordEntryRef {
	return WrappedPasswordEntryRef{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedPasswordEntryRef) Lower(value WrappedPasswordEntryRef) RustBuffer {
	return LowerIntoRustBuffer[WrappedPasswordEntryRef](c, value)
}

func (c FfiConverterTypeWrappedPasswordEntryRef) Write(writer io.Writer, value WrappedPasswordEntryRef) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Group)
}

type FfiDestroyerTypeWrappedPasswordEntryRef struct{}

func (_ FfiDestroyerTypeWrappedPasswordEntryRef) Destroy(value WrappedPasswordEntryRef) {
	value.Destroy()
}

type WrappedPasswordSiteCounts struct {
	WebsiteMetaCount  uint64
	PasswordCount     uint64
	PasswordMetaCount uint64
	PasskeyCount      uint64
}

func (r *WrappedPasswordSiteCounts) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.WebsiteMetaCount)
	FfiDestroyerUint64{}.Destroy(r.PasswordCount)
	FfiDestroyerUint64{}.Destroy(r.PasswordMetaCount)
	FfiDestroyerUint64{}.Destroy(r.PasskeyCount)
}

type FfiConverterTypeWrappedPasswordSiteCounts struct{}

var FfiConverterTypeWrappedPasswordSiteCountsINSTANCE = FfiConverterTypeWrappedPasswordSiteCounts{}

func (c FfiConverterTypeWrappedPasswordSiteCounts) Lift(rb RustBufferI) WrappedPasswordSiteCounts {
	return LiftFromRustBuffer[WrappedPasswordSiteCounts](c, rb)
}

func (c FfiConverterTypeWrappedPasswordSiteCounts) Read(reader io.Reader) WrappedPasswordSiteCounts {
	return WrappedPasswordSiteCounts{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedPasswordSiteCounts) Lower(value WrappedPasswordSiteCounts) RustBuffer {
	return LowerIntoRustBuffer[WrappedPasswordSiteCounts](c, value)
}

func (c FfiConverterTypeWrappedPasswordSiteCounts) Write(writer io.Writer, value WrappedPasswordSiteCounts) {
	FfiConverterUint64INSTANCE.Write(writer, value.WebsiteMetaCount)
	FfiConverterUint64INSTANCE.Write(writer, value.PasswordCount)
	FfiConverterUint64INSTANCE.Write(writer, value.PasswordMetaCount)
	FfiConverterUint64INSTANCE.Write(writer, value.PasskeyCount)
}

type FfiDestroyerTypeWrappedPasswordSiteCounts struct{}

func (_ FfiDestroyerTypeWrappedPasswordSiteCounts) Destroy(value WrappedPasswordSiteCounts) {
	value.Destroy()
}

type WrappedProfileRecord struct {
	DisplayName string
	FirstName   string
	LastName    string
	Avatar      *[]byte
}

func (r *WrappedProfileRecord) Destroy() {
	FfiDestroyerString{}.Destroy(r.DisplayName)
	FfiDestroyerString{}.Destroy(r.FirstName)
	FfiDestroyerString{}.Destroy(r.LastName)
	FfiDestroyerOptionalBytes{}.Destroy(r.Avatar)
}

type FfiConverterTypeWrappedProfileRecord struct{}

var FfiConverterTypeWrappedProfileRecordINSTANCE = FfiConverterTypeWrappedProfileRecord{}

func (c FfiConverterTypeWrappedProfileRecord) Lift(rb RustBufferI) WrappedProfileRecord {
	return LiftFromRustBuffer[WrappedProfileRecord](c, rb)
}

func (c FfiConverterTypeWrappedProfileRecord) Read(reader io.Reader) WrappedProfileRecord {
	return WrappedProfileRecord{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedProfileRecord) Lower(value WrappedProfileRecord) RustBuffer {
	return LowerIntoRustBuffer[WrappedProfileRecord](c, value)
}

func (c FfiConverterTypeWrappedProfileRecord) Write(writer io.Writer, value WrappedProfileRecord) {
	FfiConverterStringINSTANCE.Write(writer, value.DisplayName)
	FfiConverterStringINSTANCE.Write(writer, value.FirstName)
	FfiConverterStringINSTANCE.Write(writer, value.LastName)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Avatar)
}

type FfiDestroyerTypeWrappedProfileRecord struct{}

func (_ FfiDestroyerTypeWrappedProfileRecord) Destroy(value WrappedProfileRecord) {
	value.Destroy()
}

type WrappedShareProfileData struct {
	CloudKitRecordKey           string
	CloudKitDecryptionRecordKey []byte
	LowResWallpaperTag          *[]byte
	WallpaperTag                *[]byte
	MessageTag                  *[]byte
}

func (r *WrappedShareProfileData) Destroy() {
	FfiDestroyerString{}.Destroy(r.CloudKitRecordKey)
	FfiDestroyerBytes{}.Destroy(r.CloudKitDecryptionRecordKey)
	FfiDestroyerOptionalBytes{}.Destroy(r.LowResWallpaperTag)
	FfiDestroyerOptionalBytes{}.Destroy(r.WallpaperTag)
	FfiDestroyerOptionalBytes{}.Destroy(r.MessageTag)
}

type FfiConverterTypeWrappedShareProfileData struct{}

var FfiConverterTypeWrappedShareProfileDataINSTANCE = FfiConverterTypeWrappedShareProfileData{}

func (c FfiConverterTypeWrappedShareProfileData) Lift(rb RustBufferI) WrappedShareProfileData {
	return LiftFromRustBuffer[WrappedShareProfileData](c, rb)
}

func (c FfiConverterTypeWrappedShareProfileData) Read(reader io.Reader) WrappedShareProfileData {
	return WrappedShareProfileData{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedShareProfileData) Lower(value WrappedShareProfileData) RustBuffer {
	return LowerIntoRustBuffer[WrappedShareProfileData](c, value)
}

func (c FfiConverterTypeWrappedShareProfileData) Write(writer io.Writer, value WrappedShareProfileData) {
	FfiConverterStringINSTANCE.Write(writer, value.CloudKitRecordKey)
	FfiConverterBytesINSTANCE.Write(writer, value.CloudKitDecryptionRecordKey)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.LowResWallpaperTag)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.WallpaperTag)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.MessageTag)
}

type FfiDestroyerTypeWrappedShareProfileData struct{}

func (_ FfiDestroyerTypeWrappedShareProfileData) Destroy(value WrappedShareProfileData) {
	value.Destroy()
}

type WrappedStatusKitInviteHandle struct {
	Handle       string
	AllowedModes []string
}

func (r *WrappedStatusKitInviteHandle) Destroy() {
	FfiDestroyerString{}.Destroy(r.Handle)
	FfiDestroyerSequenceString{}.Destroy(r.AllowedModes)
}

type FfiConverterTypeWrappedStatusKitInviteHandle struct{}

var FfiConverterTypeWrappedStatusKitInviteHandleINSTANCE = FfiConverterTypeWrappedStatusKitInviteHandle{}

func (c FfiConverterTypeWrappedStatusKitInviteHandle) Lift(rb RustBufferI) WrappedStatusKitInviteHandle {
	return LiftFromRustBuffer[WrappedStatusKitInviteHandle](c, rb)
}

func (c FfiConverterTypeWrappedStatusKitInviteHandle) Read(reader io.Reader) WrappedStatusKitInviteHandle {
	return WrappedStatusKitInviteHandle{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterSequenceStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedStatusKitInviteHandle) Lower(value WrappedStatusKitInviteHandle) RustBuffer {
	return LowerIntoRustBuffer[WrappedStatusKitInviteHandle](c, value)
}

func (c FfiConverterTypeWrappedStatusKitInviteHandle) Write(writer io.Writer, value WrappedStatusKitInviteHandle) {
	FfiConverterStringINSTANCE.Write(writer, value.Handle)
	FfiConverterSequenceStringINSTANCE.Write(writer, value.AllowedModes)
}

type FfiDestroyerTypeWrappedStatusKitInviteHandle struct{}

func (_ FfiDestroyerTypeWrappedStatusKitInviteHandle) Destroy(value WrappedStatusKitInviteHandle) {
	value.Destroy()
}

type WrappedStickerExtension struct {
	MsgWidth    float64
	Rotation    float64
	Sai         uint64
	Scale       float64
	Sli         uint64
	NormalizedX float64
	NormalizedY float64
	Version     uint64
	Hash        string
	Safi        uint64
	EffectType  int64
	StickerId   string
}

func (r *WrappedStickerExtension) Destroy() {
	FfiDestroyerFloat64{}.Destroy(r.MsgWidth)
	FfiDestroyerFloat64{}.Destroy(r.Rotation)
	FfiDestroyerUint64{}.Destroy(r.Sai)
	FfiDestroyerFloat64{}.Destroy(r.Scale)
	FfiDestroyerUint64{}.Destroy(r.Sli)
	FfiDestroyerFloat64{}.Destroy(r.NormalizedX)
	FfiDestroyerFloat64{}.Destroy(r.NormalizedY)
	FfiDestroyerUint64{}.Destroy(r.Version)
	FfiDestroyerString{}.Destroy(r.Hash)
	FfiDestroyerUint64{}.Destroy(r.Safi)
	FfiDestroyerInt64{}.Destroy(r.EffectType)
	FfiDestroyerString{}.Destroy(r.StickerId)
}

type FfiConverterTypeWrappedStickerExtension struct{}

var FfiConverterTypeWrappedStickerExtensionINSTANCE = FfiConverterTypeWrappedStickerExtension{}

func (c FfiConverterTypeWrappedStickerExtension) Lift(rb RustBufferI) WrappedStickerExtension {
	return LiftFromRustBuffer[WrappedStickerExtension](c, rb)
}

func (c FfiConverterTypeWrappedStickerExtension) Read(reader io.Reader) WrappedStickerExtension {
	return WrappedStickerExtension{
		FfiConverterFloat64INSTANCE.Read(reader),
		FfiConverterFloat64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterFloat64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterFloat64INSTANCE.Read(reader),
		FfiConverterFloat64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterInt64INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterTypeWrappedStickerExtension) Lower(value WrappedStickerExtension) RustBuffer {
	return LowerIntoRustBuffer[WrappedStickerExtension](c, value)
}

func (c FfiConverterTypeWrappedStickerExtension) Write(writer io.Writer, value WrappedStickerExtension) {
	FfiConverterFloat64INSTANCE.Write(writer, value.MsgWidth)
	FfiConverterFloat64INSTANCE.Write(writer, value.Rotation)
	FfiConverterUint64INSTANCE.Write(writer, value.Sai)
	FfiConverterFloat64INSTANCE.Write(writer, value.Scale)
	FfiConverterUint64INSTANCE.Write(writer, value.Sli)
	FfiConverterFloat64INSTANCE.Write(writer, value.NormalizedX)
	FfiConverterFloat64INSTANCE.Write(writer, value.NormalizedY)
	FfiConverterUint64INSTANCE.Write(writer, value.Version)
	FfiConverterStringINSTANCE.Write(writer, value.Hash)
	FfiConverterUint64INSTANCE.Write(writer, value.Safi)
	FfiConverterInt64INSTANCE.Write(writer, value.EffectType)
	FfiConverterStringINSTANCE.Write(writer, value.StickerId)
}

type FfiDestroyerTypeWrappedStickerExtension struct{}

func (_ FfiDestroyerTypeWrappedStickerExtension) Destroy(value WrappedStickerExtension) {
	value.Destroy()
}

type WrappedError struct {
	err error
}

func (err WrappedError) Error() string {
	return fmt.Sprintf("WrappedError: %s", err.err.Error())
}

func (err WrappedError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrWrappedErrorGenericError = fmt.Errorf("WrappedErrorGenericError")

// Variant structs
type WrappedErrorGenericError struct {
	Msg string
}

func NewWrappedErrorGenericError(
	msg string,
) *WrappedError {
	return &WrappedError{
		err: &WrappedErrorGenericError{
			Msg: msg,
		},
	}
}

func (err WrappedErrorGenericError) Error() string {
	return fmt.Sprint("GenericError",
		": ",

		"Msg=",
		err.Msg,
	)
}

func (self WrappedErrorGenericError) Is(target error) bool {
	return target == ErrWrappedErrorGenericError
}

type FfiConverterTypeWrappedError struct{}

var FfiConverterTypeWrappedErrorINSTANCE = FfiConverterTypeWrappedError{}

func (c FfiConverterTypeWrappedError) Lift(eb RustBufferI) error {
	return LiftFromRustBuffer[*WrappedError](c, eb)
}

func (c FfiConverterTypeWrappedError) Lower(value *WrappedError) RustBuffer {
	return LowerIntoRustBuffer[*WrappedError](c, value)
}

func (c FfiConverterTypeWrappedError) Read(reader io.Reader) *WrappedError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &WrappedError{&WrappedErrorGenericError{
			Msg: FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterTypeWrappedError.Read()", errorID))
	}
}

func (c FfiConverterTypeWrappedError) Write(writer io.Writer, value *WrappedError) {
	switch variantValue := value.err.(type) {
	case *WrappedErrorGenericError:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Msg)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterTypeWrappedError.Write", value))
	}
}

type uniffiCallbackResult C.int32_t

const (
	uniffiIdxCallbackFree               uniffiCallbackResult = 0
	uniffiCallbackResultSuccess         uniffiCallbackResult = 0
	uniffiCallbackResultError           uniffiCallbackResult = 1
	uniffiCallbackUnexpectedResultError uniffiCallbackResult = 2
	uniffiCallbackCancelled             uniffiCallbackResult = 3
)

type concurrentHandleMap[T any] struct {
	handles       map[uint64]T
	currentHandle uint64
	lock          sync.RWMutex
}

func newConcurrentHandleMap[T any]() *concurrentHandleMap[T] {
	return &concurrentHandleMap[T]{
		handles: map[uint64]T{},
	}
}

func (cm *concurrentHandleMap[T]) insert(obj T) uint64 {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.currentHandle = cm.currentHandle + 1
	cm.handles[cm.currentHandle] = obj
	return cm.currentHandle
}

func (cm *concurrentHandleMap[T]) remove(handle uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	delete(cm.handles, handle)
}

func (cm *concurrentHandleMap[T]) tryGet(handle uint64) (T, bool) {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	val, ok := cm.handles[handle]
	return val, ok
}

type FfiConverterCallbackInterface[CallbackInterface any] struct {
	handleMap *concurrentHandleMap[CallbackInterface]
}

func (c *FfiConverterCallbackInterface[CallbackInterface]) drop(handle uint64) RustBuffer {
	c.handleMap.remove(handle)
	return RustBuffer{}
}

func (c *FfiConverterCallbackInterface[CallbackInterface]) Lift(handle uint64) CallbackInterface {
	val, ok := c.handleMap.tryGet(handle)
	if !ok {
		panic(fmt.Errorf("no callback in handle map: %d", handle))
	}
	return val
}

func (c *FfiConverterCallbackInterface[CallbackInterface]) Read(reader io.Reader) CallbackInterface {
	return c.Lift(readUint64(reader))
}

func (c *FfiConverterCallbackInterface[CallbackInterface]) Lower(value CallbackInterface) C.uint64_t {
	return C.uint64_t(c.handleMap.insert(value))
}

func (c *FfiConverterCallbackInterface[CallbackInterface]) Write(writer io.Writer, value CallbackInterface) {
	writeUint64(writer, uint64(c.Lower(value)))
}

type MessageCallback interface {
	OnMessage(msg WrappedMessage)
}

// foreignCallbackCallbackInterfaceMessageCallback cannot be callable be a compiled function at a same time
type foreignCallbackCallbackInterfaceMessageCallback struct{}

//export rustpushgo_cgo_MessageCallback
func rustpushgo_cgo_MessageCallback(handle C.uint64_t, method C.int32_t, argsPtr *C.uint8_t, argsLen C.int32_t, outBuf *C.RustBuffer) C.int32_t {
	cb := FfiConverterCallbackInterfaceMessageCallbackINSTANCE.Lift(uint64(handle))
	switch method {
	case 0:
		// 0 means Rust is done with the callback, and the callback
		// can be dropped by the foreign language.
		*outBuf = rustBufferToC(FfiConverterCallbackInterfaceMessageCallbackINSTANCE.drop(uint64(handle)))
		// See docs of ForeignCallback in `uniffi/src/ffi/foreigncallbacks.rs`
		return C.int32_t(uniffiIdxCallbackFree)

	case 1:
		var result uniffiCallbackResult
		args := unsafe.Slice((*byte)(argsPtr), argsLen)
		result = foreignCallbackCallbackInterfaceMessageCallback{}.InvokeOnMessage(cb, args, outBuf)
		return C.int32_t(result)

	default:
		// This should never happen, because an out of bounds method index won't
		// ever be used. Once we can catch errors, we should return an InternalException.
		// https://github.com/mozilla/uniffi-rs/issues/351
		return C.int32_t(uniffiCallbackUnexpectedResultError)
	}
}

func (foreignCallbackCallbackInterfaceMessageCallback) InvokeOnMessage(callback MessageCallback, args []byte, outBuf *C.RustBuffer) uniffiCallbackResult {
	reader := bytes.NewReader(args)
	callback.OnMessage(FfiConverterTypeWrappedMessageINSTANCE.Read(reader))

	return uniffiCallbackResultSuccess
}

type FfiConverterCallbackInterfaceMessageCallback struct {
	FfiConverterCallbackInterface[MessageCallback]
}

var FfiConverterCallbackInterfaceMessageCallbackINSTANCE = &FfiConverterCallbackInterfaceMessageCallback{
	FfiConverterCallbackInterface: FfiConverterCallbackInterface[MessageCallback]{
		handleMap: newConcurrentHandleMap[MessageCallback](),
	},
}

// This is a static function because only 1 instance is supported for registering
func (c *FfiConverterCallbackInterfaceMessageCallback) register() {
	rustCall(func(status *C.RustCallStatus) int32 {
		C.uniffi_rustpushgo_fn_init_callback_messagecallback(C.ForeignCallback(C.rustpushgo_cgo_MessageCallback), status)
		return 0
	})
}

type FfiDestroyerCallbackInterfaceMessageCallback struct{}

func (FfiDestroyerCallbackInterfaceMessageCallback) Destroy(value MessageCallback) {
}

type StatusCallback interface {
	OnStatusUpdate(user string, mode *string, available bool)

	OnKeysReceived()
}

// foreignCallbackCallbackInterfaceStatusCallback cannot be callable be a compiled function at a same time
type foreignCallbackCallbackInterfaceStatusCallback struct{}

//export rustpushgo_cgo_StatusCallback
func rustpushgo_cgo_StatusCallback(handle C.uint64_t, method C.int32_t, argsPtr *C.uint8_t, argsLen C.int32_t, outBuf *C.RustBuffer) C.int32_t {
	cb := FfiConverterCallbackInterfaceStatusCallbackINSTANCE.Lift(uint64(handle))
	switch method {
	case 0:
		// 0 means Rust is done with the callback, and the callback
		// can be dropped by the foreign language.
		*outBuf = rustBufferToC(FfiConverterCallbackInterfaceStatusCallbackINSTANCE.drop(uint64(handle)))
		// See docs of ForeignCallback in `uniffi/src/ffi/foreigncallbacks.rs`
		return C.int32_t(uniffiIdxCallbackFree)

	case 1:
		var result uniffiCallbackResult
		args := unsafe.Slice((*byte)(argsPtr), argsLen)
		result = foreignCallbackCallbackInterfaceStatusCallback{}.InvokeOnStatusUpdate(cb, args, outBuf)
		return C.int32_t(result)
	case 2:
		var result uniffiCallbackResult
		args := unsafe.Slice((*byte)(argsPtr), argsLen)
		result = foreignCallbackCallbackInterfaceStatusCallback{}.InvokeOnKeysReceived(cb, args, outBuf)
		return C.int32_t(result)

	default:
		// This should never happen, because an out of bounds method index won't
		// ever be used. Once we can catch errors, we should return an InternalException.
		// https://github.com/mozilla/uniffi-rs/issues/351
		return C.int32_t(uniffiCallbackUnexpectedResultError)
	}
}

func (foreignCallbackCallbackInterfaceStatusCallback) InvokeOnStatusUpdate(callback StatusCallback, args []byte, outBuf *C.RustBuffer) uniffiCallbackResult {
	reader := bytes.NewReader(args)
	callback.OnStatusUpdate(FfiConverterStringINSTANCE.Read(reader), FfiConverterOptionalStringINSTANCE.Read(reader), FfiConverterBoolINSTANCE.Read(reader))

	return uniffiCallbackResultSuccess
}
func (foreignCallbackCallbackInterfaceStatusCallback) InvokeOnKeysReceived(callback StatusCallback, args []byte, outBuf *C.RustBuffer) uniffiCallbackResult {
	callback.OnKeysReceived()

	return uniffiCallbackResultSuccess
}

type FfiConverterCallbackInterfaceStatusCallback struct {
	FfiConverterCallbackInterface[StatusCallback]
}

var FfiConverterCallbackInterfaceStatusCallbackINSTANCE = &FfiConverterCallbackInterfaceStatusCallback{
	FfiConverterCallbackInterface: FfiConverterCallbackInterface[StatusCallback]{
		handleMap: newConcurrentHandleMap[StatusCallback](),
	},
}

// This is a static function because only 1 instance is supported for registering
func (c *FfiConverterCallbackInterfaceStatusCallback) register() {
	rustCall(func(status *C.RustCallStatus) int32 {
		C.uniffi_rustpushgo_fn_init_callback_statuscallback(C.ForeignCallback(C.rustpushgo_cgo_StatusCallback), status)
		return 0
	})
}

type FfiDestroyerCallbackInterfaceStatusCallback struct{}

func (FfiDestroyerCallbackInterfaceStatusCallback) Destroy(value StatusCallback) {
}

type UpdateUsersCallback interface {
	UpdateUsers(users *WrappedIdsUsers)
}

// foreignCallbackCallbackInterfaceUpdateUsersCallback cannot be callable be a compiled function at a same time
type foreignCallbackCallbackInterfaceUpdateUsersCallback struct{}

//export rustpushgo_cgo_UpdateUsersCallback
func rustpushgo_cgo_UpdateUsersCallback(handle C.uint64_t, method C.int32_t, argsPtr *C.uint8_t, argsLen C.int32_t, outBuf *C.RustBuffer) C.int32_t {
	cb := FfiConverterCallbackInterfaceUpdateUsersCallbackINSTANCE.Lift(uint64(handle))
	switch method {
	case 0:
		// 0 means Rust is done with the callback, and the callback
		// can be dropped by the foreign language.
		*outBuf = rustBufferToC(FfiConverterCallbackInterfaceUpdateUsersCallbackINSTANCE.drop(uint64(handle)))
		// See docs of ForeignCallback in `uniffi/src/ffi/foreigncallbacks.rs`
		return C.int32_t(uniffiIdxCallbackFree)

	case 1:
		var result uniffiCallbackResult
		args := unsafe.Slice((*byte)(argsPtr), argsLen)
		result = foreignCallbackCallbackInterfaceUpdateUsersCallback{}.InvokeUpdateUsers(cb, args, outBuf)
		return C.int32_t(result)

	default:
		// This should never happen, because an out of bounds method index won't
		// ever be used. Once we can catch errors, we should return an InternalException.
		// https://github.com/mozilla/uniffi-rs/issues/351
		return C.int32_t(uniffiCallbackUnexpectedResultError)
	}
}

func (foreignCallbackCallbackInterfaceUpdateUsersCallback) InvokeUpdateUsers(callback UpdateUsersCallback, args []byte, outBuf *C.RustBuffer) uniffiCallbackResult {
	reader := bytes.NewReader(args)
	callback.UpdateUsers(FfiConverterWrappedIDSUsersINSTANCE.Read(reader))

	return uniffiCallbackResultSuccess
}

type FfiConverterCallbackInterfaceUpdateUsersCallback struct {
	FfiConverterCallbackInterface[UpdateUsersCallback]
}

var FfiConverterCallbackInterfaceUpdateUsersCallbackINSTANCE = &FfiConverterCallbackInterfaceUpdateUsersCallback{
	FfiConverterCallbackInterface: FfiConverterCallbackInterface[UpdateUsersCallback]{
		handleMap: newConcurrentHandleMap[UpdateUsersCallback](),
	},
}

// This is a static function because only 1 instance is supported for registering
func (c *FfiConverterCallbackInterfaceUpdateUsersCallback) register() {
	rustCall(func(status *C.RustCallStatus) int32 {
		C.uniffi_rustpushgo_fn_init_callback_updateuserscallback(C.ForeignCallback(C.rustpushgo_cgo_UpdateUsersCallback), status)
		return 0
	})
}

type FfiDestroyerCallbackInterfaceUpdateUsersCallback struct{}

func (FfiDestroyerCallbackInterfaceUpdateUsersCallback) Destroy(value UpdateUsersCallback) {
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBool struct{}

var FfiConverterOptionalBoolINSTANCE = FfiConverterOptionalBool{}

func (c FfiConverterOptionalBool) Lift(rb RustBufferI) *bool {
	return LiftFromRustBuffer[*bool](c, rb)
}

func (_ FfiConverterOptionalBool) Read(reader io.Reader) *bool {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBoolINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBool) Lower(value *bool) RustBuffer {
	return LowerIntoRustBuffer[*bool](c, value)
}

func (_ FfiConverterOptionalBool) Write(writer io.Writer, value *bool) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBoolINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBool struct{}

func (_ FfiDestroyerOptionalBool) Destroy(value *bool) {
	if value != nil {
		FfiDestroyerBool{}.Destroy(*value)
	}
}

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalWrappedIDSNGMIdentity struct{}

var FfiConverterOptionalWrappedIDSNGMIdentityINSTANCE = FfiConverterOptionalWrappedIDSNGMIdentity{}

func (c FfiConverterOptionalWrappedIDSNGMIdentity) Lift(rb RustBufferI) **WrappedIdsngmIdentity {
	return LiftFromRustBuffer[**WrappedIdsngmIdentity](c, rb)
}

func (_ FfiConverterOptionalWrappedIDSNGMIdentity) Read(reader io.Reader) **WrappedIdsngmIdentity {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterWrappedIDSNGMIdentityINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalWrappedIDSNGMIdentity) Lower(value **WrappedIdsngmIdentity) RustBuffer {
	return LowerIntoRustBuffer[**WrappedIdsngmIdentity](c, value)
}

func (_ FfiConverterOptionalWrappedIDSNGMIdentity) Write(writer io.Writer, value **WrappedIdsngmIdentity) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterWrappedIDSNGMIdentityINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalWrappedIdsngmIdentity struct{}

func (_ FfiDestroyerOptionalWrappedIdsngmIdentity) Destroy(value **WrappedIdsngmIdentity) {
	if value != nil {
		FfiDestroyerWrappedIdsngmIdentity{}.Destroy(*value)
	}
}

type FfiConverterOptionalWrappedIDSUsers struct{}

var FfiConverterOptionalWrappedIDSUsersINSTANCE = FfiConverterOptionalWrappedIDSUsers{}

func (c FfiConverterOptionalWrappedIDSUsers) Lift(rb RustBufferI) **WrappedIdsUsers {
	return LiftFromRustBuffer[**WrappedIdsUsers](c, rb)
}

func (_ FfiConverterOptionalWrappedIDSUsers) Read(reader io.Reader) **WrappedIdsUsers {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterWrappedIDSUsersINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalWrappedIDSUsers) Lower(value **WrappedIdsUsers) RustBuffer {
	return LowerIntoRustBuffer[**WrappedIdsUsers](c, value)
}

func (_ FfiConverterOptionalWrappedIDSUsers) Write(writer io.Writer, value **WrappedIdsUsers) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterWrappedIDSUsersINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalWrappedIdsUsers struct{}

func (_ FfiDestroyerOptionalWrappedIdsUsers) Destroy(value **WrappedIdsUsers) {
	if value != nil {
		FfiDestroyerWrappedIdsUsers{}.Destroy(*value)
	}
}

type FfiConverterOptionalWrappedTokenProvider struct{}

var FfiConverterOptionalWrappedTokenProviderINSTANCE = FfiConverterOptionalWrappedTokenProvider{}

func (c FfiConverterOptionalWrappedTokenProvider) Lift(rb RustBufferI) **WrappedTokenProvider {
	return LiftFromRustBuffer[**WrappedTokenProvider](c, rb)
}

func (_ FfiConverterOptionalWrappedTokenProvider) Read(reader io.Reader) **WrappedTokenProvider {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterWrappedTokenProviderINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalWrappedTokenProvider) Lower(value **WrappedTokenProvider) RustBuffer {
	return LowerIntoRustBuffer[**WrappedTokenProvider](c, value)
}

func (_ FfiConverterOptionalWrappedTokenProvider) Write(writer io.Writer, value **WrappedTokenProvider) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterWrappedTokenProviderINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalWrappedTokenProvider struct{}

func (_ FfiDestroyerOptionalWrappedTokenProvider) Destroy(value **WrappedTokenProvider) {
	if value != nil {
		FfiDestroyerWrappedTokenProvider{}.Destroy(*value)
	}
}

type FfiConverterOptionalTypeAccountPersistData struct{}

var FfiConverterOptionalTypeAccountPersistDataINSTANCE = FfiConverterOptionalTypeAccountPersistData{}

func (c FfiConverterOptionalTypeAccountPersistData) Lift(rb RustBufferI) *AccountPersistData {
	return LiftFromRustBuffer[*AccountPersistData](c, rb)
}

func (_ FfiConverterOptionalTypeAccountPersistData) Read(reader io.Reader) *AccountPersistData {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterTypeAccountPersistDataINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalTypeAccountPersistData) Lower(value *AccountPersistData) RustBuffer {
	return LowerIntoRustBuffer[*AccountPersistData](c, value)
}

func (_ FfiConverterOptionalTypeAccountPersistData) Write(writer io.Writer, value *AccountPersistData) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterTypeAccountPersistDataINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalTypeAccountPersistData struct{}

func (_ FfiDestroyerOptionalTypeAccountPersistData) Destroy(value *AccountPersistData) {
	if value != nil {
		FfiDestroyerTypeAccountPersistData{}.Destroy(*value)
	}
}

type FfiConverterOptionalTypeWrappedShareProfileData struct{}

var FfiConverterOptionalTypeWrappedShareProfileDataINSTANCE = FfiConverterOptionalTypeWrappedShareProfileData{}

func (c FfiConverterOptionalTypeWrappedShareProfileData) Lift(rb RustBufferI) *WrappedShareProfileData {
	return LiftFromRustBuffer[*WrappedShareProfileData](c, rb)
}

func (_ FfiConverterOptionalTypeWrappedShareProfileData) Read(reader io.Reader) *WrappedShareProfileData {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterTypeWrappedShareProfileDataINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalTypeWrappedShareProfileData) Lower(value *WrappedShareProfileData) RustBuffer {
	return LowerIntoRustBuffer[*WrappedShareProfileData](c, value)
}

func (_ FfiConverterOptionalTypeWrappedShareProfileData) Write(writer io.Writer, value *WrappedShareProfileData) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterTypeWrappedShareProfileDataINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalTypeWrappedShareProfileData struct{}

func (_ FfiDestroyerOptionalTypeWrappedShareProfileData) Destroy(value *WrappedShareProfileData) {
	if value != nil {
		FfiDestroyerTypeWrappedShareProfileData{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceString struct{}

var FfiConverterOptionalSequenceStringINSTANCE = FfiConverterOptionalSequenceString{}

func (c FfiConverterOptionalSequenceString) Lift(rb RustBufferI) *[]string {
	return LiftFromRustBuffer[*[]string](c, rb)
}

func (_ FfiConverterOptionalSequenceString) Read(reader io.Reader) *[]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceString) Lower(value *[]string) RustBuffer {
	return LowerIntoRustBuffer[*[]string](c, value)
}

func (_ FfiConverterOptionalSequenceString) Write(writer io.Writer, value *[]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceString struct{}

func (_ FfiDestroyerOptionalSequenceString) Destroy(value *[]string) {
	if value != nil {
		FfiDestroyerSequenceString{}.Destroy(*value)
	}
}

type FfiConverterOptionalMapStringString struct{}

var FfiConverterOptionalMapStringStringINSTANCE = FfiConverterOptionalMapStringString{}

func (c FfiConverterOptionalMapStringString) Lift(rb RustBufferI) *map[string]string {
	return LiftFromRustBuffer[*map[string]string](c, rb)
}

func (_ FfiConverterOptionalMapStringString) Read(reader io.Reader) *map[string]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMapStringStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMapStringString) Lower(value *map[string]string) RustBuffer {
	return LowerIntoRustBuffer[*map[string]string](c, value)
}

func (_ FfiConverterOptionalMapStringString) Write(writer io.Writer, value *map[string]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMapStringStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMapStringString struct{}

func (_ FfiDestroyerOptionalMapStringString) Destroy(value *map[string]string) {
	if value != nil {
		FfiDestroyerMapStringString{}.Destroy(*value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeEscrowDeviceInfo struct{}

var FfiConverterSequenceTypeEscrowDeviceInfoINSTANCE = FfiConverterSequenceTypeEscrowDeviceInfo{}

func (c FfiConverterSequenceTypeEscrowDeviceInfo) Lift(rb RustBufferI) []EscrowDeviceInfo {
	return LiftFromRustBuffer[[]EscrowDeviceInfo](c, rb)
}

func (c FfiConverterSequenceTypeEscrowDeviceInfo) Read(reader io.Reader) []EscrowDeviceInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]EscrowDeviceInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeEscrowDeviceInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeEscrowDeviceInfo) Lower(value []EscrowDeviceInfo) RustBuffer {
	return LowerIntoRustBuffer[[]EscrowDeviceInfo](c, value)
}

func (c FfiConverterSequenceTypeEscrowDeviceInfo) Write(writer io.Writer, value []EscrowDeviceInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]EscrowDeviceInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeEscrowDeviceInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeEscrowDeviceInfo struct{}

func (FfiDestroyerSequenceTypeEscrowDeviceInfo) Destroy(sequence []EscrowDeviceInfo) {
	for _, value := range sequence {
		FfiDestroyerTypeEscrowDeviceInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeSharedAlbumInfo struct{}

var FfiConverterSequenceTypeSharedAlbumInfoINSTANCE = FfiConverterSequenceTypeSharedAlbumInfo{}

func (c FfiConverterSequenceTypeSharedAlbumInfo) Lift(rb RustBufferI) []SharedAlbumInfo {
	return LiftFromRustBuffer[[]SharedAlbumInfo](c, rb)
}

func (c FfiConverterSequenceTypeSharedAlbumInfo) Read(reader io.Reader) []SharedAlbumInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SharedAlbumInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeSharedAlbumInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeSharedAlbumInfo) Lower(value []SharedAlbumInfo) RustBuffer {
	return LowerIntoRustBuffer[[]SharedAlbumInfo](c, value)
}

func (c FfiConverterSequenceTypeSharedAlbumInfo) Write(writer io.Writer, value []SharedAlbumInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]SharedAlbumInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeSharedAlbumInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeSharedAlbumInfo struct{}

func (FfiDestroyerSequenceTypeSharedAlbumInfo) Destroy(sequence []SharedAlbumInfo) {
	for _, value := range sequence {
		FfiDestroyerTypeSharedAlbumInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeSharedAssetInfo struct{}

var FfiConverterSequenceTypeSharedAssetInfoINSTANCE = FfiConverterSequenceTypeSharedAssetInfo{}

func (c FfiConverterSequenceTypeSharedAssetInfo) Lift(rb RustBufferI) []SharedAssetInfo {
	return LiftFromRustBuffer[[]SharedAssetInfo](c, rb)
}

func (c FfiConverterSequenceTypeSharedAssetInfo) Read(reader io.Reader) []SharedAssetInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SharedAssetInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeSharedAssetInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeSharedAssetInfo) Lower(value []SharedAssetInfo) RustBuffer {
	return LowerIntoRustBuffer[[]SharedAssetInfo](c, value)
}

func (c FfiConverterSequenceTypeSharedAssetInfo) Write(writer io.Writer, value []SharedAssetInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]SharedAssetInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeSharedAssetInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeSharedAssetInfo struct{}

func (FfiDestroyerSequenceTypeSharedAssetInfo) Destroy(sequence []SharedAssetInfo) {
	for _, value := range sequence {
		FfiDestroyerTypeSharedAssetInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedAttachment struct{}

var FfiConverterSequenceTypeWrappedAttachmentINSTANCE = FfiConverterSequenceTypeWrappedAttachment{}

func (c FfiConverterSequenceTypeWrappedAttachment) Lift(rb RustBufferI) []WrappedAttachment {
	return LiftFromRustBuffer[[]WrappedAttachment](c, rb)
}

func (c FfiConverterSequenceTypeWrappedAttachment) Read(reader io.Reader) []WrappedAttachment {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedAttachment, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedAttachmentINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedAttachment) Lower(value []WrappedAttachment) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedAttachment](c, value)
}

func (c FfiConverterSequenceTypeWrappedAttachment) Write(writer io.Writer, value []WrappedAttachment) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedAttachment is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedAttachmentINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedAttachment struct{}

func (FfiDestroyerSequenceTypeWrappedAttachment) Destroy(sequence []WrappedAttachment) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedAttachment{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedCloudAttachmentInfo struct{}

var FfiConverterSequenceTypeWrappedCloudAttachmentInfoINSTANCE = FfiConverterSequenceTypeWrappedCloudAttachmentInfo{}

func (c FfiConverterSequenceTypeWrappedCloudAttachmentInfo) Lift(rb RustBufferI) []WrappedCloudAttachmentInfo {
	return LiftFromRustBuffer[[]WrappedCloudAttachmentInfo](c, rb)
}

func (c FfiConverterSequenceTypeWrappedCloudAttachmentInfo) Read(reader io.Reader) []WrappedCloudAttachmentInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedCloudAttachmentInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedCloudAttachmentInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedCloudAttachmentInfo) Lower(value []WrappedCloudAttachmentInfo) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedCloudAttachmentInfo](c, value)
}

func (c FfiConverterSequenceTypeWrappedCloudAttachmentInfo) Write(writer io.Writer, value []WrappedCloudAttachmentInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedCloudAttachmentInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedCloudAttachmentInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedCloudAttachmentInfo struct{}

func (FfiDestroyerSequenceTypeWrappedCloudAttachmentInfo) Destroy(sequence []WrappedCloudAttachmentInfo) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedCloudAttachmentInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedCloudSyncChat struct{}

var FfiConverterSequenceTypeWrappedCloudSyncChatINSTANCE = FfiConverterSequenceTypeWrappedCloudSyncChat{}

func (c FfiConverterSequenceTypeWrappedCloudSyncChat) Lift(rb RustBufferI) []WrappedCloudSyncChat {
	return LiftFromRustBuffer[[]WrappedCloudSyncChat](c, rb)
}

func (c FfiConverterSequenceTypeWrappedCloudSyncChat) Read(reader io.Reader) []WrappedCloudSyncChat {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedCloudSyncChat, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedCloudSyncChatINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedCloudSyncChat) Lower(value []WrappedCloudSyncChat) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedCloudSyncChat](c, value)
}

func (c FfiConverterSequenceTypeWrappedCloudSyncChat) Write(writer io.Writer, value []WrappedCloudSyncChat) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedCloudSyncChat is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedCloudSyncChatINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedCloudSyncChat struct{}

func (FfiDestroyerSequenceTypeWrappedCloudSyncChat) Destroy(sequence []WrappedCloudSyncChat) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedCloudSyncChat{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedCloudSyncMessage struct{}

var FfiConverterSequenceTypeWrappedCloudSyncMessageINSTANCE = FfiConverterSequenceTypeWrappedCloudSyncMessage{}

func (c FfiConverterSequenceTypeWrappedCloudSyncMessage) Lift(rb RustBufferI) []WrappedCloudSyncMessage {
	return LiftFromRustBuffer[[]WrappedCloudSyncMessage](c, rb)
}

func (c FfiConverterSequenceTypeWrappedCloudSyncMessage) Read(reader io.Reader) []WrappedCloudSyncMessage {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedCloudSyncMessage, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedCloudSyncMessageINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedCloudSyncMessage) Lower(value []WrappedCloudSyncMessage) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedCloudSyncMessage](c, value)
}

func (c FfiConverterSequenceTypeWrappedCloudSyncMessage) Write(writer io.Writer, value []WrappedCloudSyncMessage) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedCloudSyncMessage is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedCloudSyncMessageINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedCloudSyncMessage struct{}

func (FfiDestroyerSequenceTypeWrappedCloudSyncMessage) Destroy(sequence []WrappedCloudSyncMessage) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedCloudSyncMessage{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedLetMeInRequest struct{}

var FfiConverterSequenceTypeWrappedLetMeInRequestINSTANCE = FfiConverterSequenceTypeWrappedLetMeInRequest{}

func (c FfiConverterSequenceTypeWrappedLetMeInRequest) Lift(rb RustBufferI) []WrappedLetMeInRequest {
	return LiftFromRustBuffer[[]WrappedLetMeInRequest](c, rb)
}

func (c FfiConverterSequenceTypeWrappedLetMeInRequest) Read(reader io.Reader) []WrappedLetMeInRequest {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedLetMeInRequest, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedLetMeInRequestINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedLetMeInRequest) Lower(value []WrappedLetMeInRequest) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedLetMeInRequest](c, value)
}

func (c FfiConverterSequenceTypeWrappedLetMeInRequest) Write(writer io.Writer, value []WrappedLetMeInRequest) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedLetMeInRequest is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedLetMeInRequestINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedLetMeInRequest struct{}

func (FfiDestroyerSequenceTypeWrappedLetMeInRequest) Destroy(sequence []WrappedLetMeInRequest) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedLetMeInRequest{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedPasswordEntryRef struct{}

var FfiConverterSequenceTypeWrappedPasswordEntryRefINSTANCE = FfiConverterSequenceTypeWrappedPasswordEntryRef{}

func (c FfiConverterSequenceTypeWrappedPasswordEntryRef) Lift(rb RustBufferI) []WrappedPasswordEntryRef {
	return LiftFromRustBuffer[[]WrappedPasswordEntryRef](c, rb)
}

func (c FfiConverterSequenceTypeWrappedPasswordEntryRef) Read(reader io.Reader) []WrappedPasswordEntryRef {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedPasswordEntryRef, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedPasswordEntryRefINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedPasswordEntryRef) Lower(value []WrappedPasswordEntryRef) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedPasswordEntryRef](c, value)
}

func (c FfiConverterSequenceTypeWrappedPasswordEntryRef) Write(writer io.Writer, value []WrappedPasswordEntryRef) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedPasswordEntryRef is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedPasswordEntryRefINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedPasswordEntryRef struct{}

func (FfiDestroyerSequenceTypeWrappedPasswordEntryRef) Destroy(sequence []WrappedPasswordEntryRef) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedPasswordEntryRef{}.Destroy(value)
	}
}

type FfiConverterSequenceTypeWrappedStatusKitInviteHandle struct{}

var FfiConverterSequenceTypeWrappedStatusKitInviteHandleINSTANCE = FfiConverterSequenceTypeWrappedStatusKitInviteHandle{}

func (c FfiConverterSequenceTypeWrappedStatusKitInviteHandle) Lift(rb RustBufferI) []WrappedStatusKitInviteHandle {
	return LiftFromRustBuffer[[]WrappedStatusKitInviteHandle](c, rb)
}

func (c FfiConverterSequenceTypeWrappedStatusKitInviteHandle) Read(reader io.Reader) []WrappedStatusKitInviteHandle {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]WrappedStatusKitInviteHandle, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterTypeWrappedStatusKitInviteHandleINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceTypeWrappedStatusKitInviteHandle) Lower(value []WrappedStatusKitInviteHandle) RustBuffer {
	return LowerIntoRustBuffer[[]WrappedStatusKitInviteHandle](c, value)
}

func (c FfiConverterSequenceTypeWrappedStatusKitInviteHandle) Write(writer io.Writer, value []WrappedStatusKitInviteHandle) {
	if len(value) > math.MaxInt32 {
		panic("[]WrappedStatusKitInviteHandle is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterTypeWrappedStatusKitInviteHandleINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceTypeWrappedStatusKitInviteHandle struct{}

func (FfiDestroyerSequenceTypeWrappedStatusKitInviteHandle) Destroy(sequence []WrappedStatusKitInviteHandle) {
	for _, value := range sequence {
		FfiDestroyerTypeWrappedStatusKitInviteHandle{}.Destroy(value)
	}
}

type FfiConverterMapStringString struct{}

var FfiConverterMapStringStringINSTANCE = FfiConverterMapStringString{}

func (c FfiConverterMapStringString) Lift(rb RustBufferI) map[string]string {
	return LiftFromRustBuffer[map[string]string](c, rb)
}

func (_ FfiConverterMapStringString) Read(reader io.Reader) map[string]string {
	result := make(map[string]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringString) Lower(value map[string]string) RustBuffer {
	return LowerIntoRustBuffer[map[string]string](c, value)
}

func (_ FfiConverterMapStringString) Write(writer io.Writer, mapValue map[string]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringString struct{}

func (_ FfiDestroyerMapStringString) Destroy(mapValue map[string]string) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
)

func uniffiRustCallAsync(
	rustFutureFunc func(*C.RustCallStatus) *C.void,
	pollFunc func(*C.void, unsafe.Pointer, *C.RustCallStatus),
	completeFunc func(*C.void, *C.RustCallStatus),
	_liftFunc func(bool),
	freeFunc func(*C.void, *C.RustCallStatus),
) {
	rustFuture, err := uniffiRustCallAsyncInner(nil, rustFutureFunc, pollFunc, freeFunc)
	if err != nil {
		panic(err)
	}
	defer rustCall(func(status *C.RustCallStatus) int {
		freeFunc(rustFuture, status)
		return 0
	})

	rustCall(func(status *C.RustCallStatus) int {
		completeFunc(rustFuture, status)
		return 0
	})
}

func uniffiRustCallAsyncWithResult[T any, U any](
	rustFutureFunc func(*C.RustCallStatus) *C.void,
	pollFunc func(*C.void, unsafe.Pointer, *C.RustCallStatus),
	completeFunc func(*C.void, *C.RustCallStatus) T,
	liftFunc func(T) U,
	freeFunc func(*C.void, *C.RustCallStatus),
) U {
	rustFuture, err := uniffiRustCallAsyncInner(nil, rustFutureFunc, pollFunc, freeFunc)
	if err != nil {
		panic(err)
	}

	defer rustCall(func(status *C.RustCallStatus) int {
		freeFunc(rustFuture, status)
		return 0
	})

	res := rustCall(func(status *C.RustCallStatus) T {
		return completeFunc(rustFuture, status)
	})
	return liftFunc(res)
}

func uniffiRustCallAsyncWithError(
	converter BufLifter[error],
	rustFutureFunc func(*C.RustCallStatus) *C.void,
	pollFunc func(*C.void, unsafe.Pointer, *C.RustCallStatus),
	completeFunc func(*C.void, *C.RustCallStatus),
	_liftFunc func(bool),
	freeFunc func(*C.void, *C.RustCallStatus),
) error {
	rustFuture, err := uniffiRustCallAsyncInner(converter, rustFutureFunc, pollFunc, freeFunc)
	if err != nil {
		return err
	}

	defer rustCall(func(status *C.RustCallStatus) int {
		freeFunc(rustFuture, status)
		return 0
	})

	_, err = rustCallWithError(converter, func(status *C.RustCallStatus) int {
		completeFunc(rustFuture, status)
		return 0
	})
	return err
}

func uniffiRustCallAsyncWithErrorAndResult[T any, U any](
	converter BufLifter[error],
	rustFutureFunc func(*C.RustCallStatus) *C.void,
	pollFunc func(*C.void, unsafe.Pointer, *C.RustCallStatus),
	completeFunc func(*C.void, *C.RustCallStatus) T,
	liftFunc func(T) U,
	freeFunc func(*C.void, *C.RustCallStatus),
) (U, error) {
	var returnValue U
	rustFuture, err := uniffiRustCallAsyncInner(converter, rustFutureFunc, pollFunc, freeFunc)
	if err != nil {
		return returnValue, err
	}

	defer rustCall(func(status *C.RustCallStatus) int {
		freeFunc(rustFuture, status)
		return 0
	})

	res, err := rustCallWithError(converter, func(status *C.RustCallStatus) T {
		return completeFunc(rustFuture, status)
	})
	if err != nil {
		return returnValue, err
	}
	return liftFunc(res), nil
}

func uniffiRustCallAsyncInner(
	converter BufLifter[error],
	rustFutureFunc func(*C.RustCallStatus) *C.void,
	pollFunc func(*C.void, unsafe.Pointer, *C.RustCallStatus),
	freeFunc func(*C.void, *C.RustCallStatus),
) (*C.void, error) {
	pollResult := int8(-1)
	waiter := make(chan int8, 1)
	chanHandle := cgo.NewHandle(waiter)

	rustFuture, err := rustCallWithError(converter, func(status *C.RustCallStatus) *C.void {
		return rustFutureFunc(status)
	})
	if err != nil {
		return nil, err
	}

	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		ptr := unsafe.Pointer(&chanHandle)
		_, err = rustCallWithError(converter, func(status *C.RustCallStatus) int {
			pollFunc(rustFuture, ptr, status)
			return 0
		})
		if err != nil {
			return nil, err
		}
		res := <-waiter
		pollResult = res
	}

	return rustFuture, nil
}

// Callback handlers for an async calls.  These are invoked by Rust when the future is ready.  They
// lift the return value or error and resume the suspended function.

//export uniffiFutureContinuationCallbackrustpushgo
func uniffiFutureContinuationCallbackrustpushgo(ptr unsafe.Pointer, pollResult C.int8_t) {
	doneHandle := *(*cgo.Handle)(ptr)
	done := doneHandle.Value().((chan int8))
	done <- int8(pollResult)
}

func uniffiInitContinuationCallback() {
	rustCall(func(uniffiStatus *C.RustCallStatus) bool {
		C.ffi_rustpushgo_rust_future_continuation_callback_set(
			C.RustFutureContinuation(C.uniffiFutureContinuationCallbackrustpushgo),
			uniffiStatus,
		)
		return false
	})
}

func Connect(config *WrappedOsConfig, state *WrappedApsState) *WrappedApsConnection {
	return uniffiRustCallAsyncWithResult(func(status *C.RustCallStatus) *C.void {
		// rustFutureFunc
		return (*C.void)(C.uniffi_rustpushgo_fn_func_connect(FfiConverterWrappedOSConfigINSTANCE.Lower(config), FfiConverterWrappedAPSStateINSTANCE.Lower(state),
			status,
		))
	},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedAPSConnectionINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func CreateConfigFromHardwareKey(base64Key string) (*WrappedOsConfig, error) {
	_uniffiRV, _uniffiErr := rustCallWithError(FfiConverterTypeWrappedError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_func_create_config_from_hardware_key(rustBufferToC(FfiConverterStringINSTANCE.Lower(base64Key)), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WrappedOsConfig
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWrappedOSConfigINSTANCE.Lift(_uniffiRV), _uniffiErr
	}
}

func CreateConfigFromHardwareKeyWithDeviceId(base64Key string, deviceId string) (*WrappedOsConfig, error) {
	_uniffiRV, _uniffiErr := rustCallWithError(FfiConverterTypeWrappedError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_func_create_config_from_hardware_key_with_device_id(rustBufferToC(FfiConverterStringINSTANCE.Lower(base64Key)), rustBufferToC(FfiConverterStringINSTANCE.Lower(deviceId)), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WrappedOsConfig
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWrappedOSConfigINSTANCE.Lift(_uniffiRV), _uniffiErr
	}
}

func CreateLocalMacosConfig() (*WrappedOsConfig, error) {
	_uniffiRV, _uniffiErr := rustCallWithError(FfiConverterTypeWrappedError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_func_create_local_macos_config(_uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WrappedOsConfig
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWrappedOSConfigINSTANCE.Lift(_uniffiRV), _uniffiErr
	}
}

func CreateLocalMacosConfigWithDeviceId(deviceId string) (*WrappedOsConfig, error) {
	_uniffiRV, _uniffiErr := rustCallWithError(FfiConverterTypeWrappedError{}, func(_uniffiStatus *C.RustCallStatus) unsafe.Pointer {
		return C.uniffi_rustpushgo_fn_func_create_local_macos_config_with_device_id(rustBufferToC(FfiConverterStringINSTANCE.Lower(deviceId)), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *WrappedOsConfig
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterWrappedOSConfigINSTANCE.Lift(_uniffiRV), _uniffiErr
	}
}

func FordKeyCacheSize() uint64 {
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_rustpushgo_fn_func_ford_key_cache_size(_uniffiStatus)
	}))
}

func InitLogger() {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_rustpushgo_fn_func_init_logger(_uniffiStatus)
		return false
	})
}

func LoginStart(appleId string, password string, config *WrappedOsConfig, connection *WrappedApsConnection) (*LoginSession, error) {
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_func_login_start(rustBufferToC(FfiConverterStringINSTANCE.Lower(appleId)), rustBufferToC(FfiConverterStringINSTANCE.Lower(password)), FfiConverterWrappedOSConfigINSTANCE.Lower(config), FfiConverterWrappedAPSConnectionINSTANCE.Lower(connection),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterLoginSessionINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func NewClient(connection *WrappedApsConnection, users *WrappedIdsUsers, identity *WrappedIdsngmIdentity, config *WrappedOsConfig, tokenProvider **WrappedTokenProvider, messageCallback MessageCallback, updateUsersCallback UpdateUsersCallback) (*Client, error) {
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_func_new_client(FfiConverterWrappedAPSConnectionINSTANCE.Lower(connection), FfiConverterWrappedIDSUsersINSTANCE.Lower(users), FfiConverterWrappedIDSNGMIdentityINSTANCE.Lower(identity), FfiConverterWrappedOSConfigINSTANCE.Lower(config), rustBufferToC(FfiConverterOptionalWrappedTokenProviderINSTANCE.Lower(tokenProvider)), FfiConverterCallbackInterfaceMessageCallbackINSTANCE.Lower(messageCallback), FfiConverterCallbackInterfaceUpdateUsersCallbackINSTANCE.Lower(updateUsersCallback),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterClientINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}

func RegisterFordKey(key []byte) {
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_rustpushgo_fn_func_register_ford_key(rustBufferToC(FfiConverterBytesINSTANCE.Lower(key)), _uniffiStatus)
		return false
	})
}

func RestoreTokenProvider(config *WrappedOsConfig, connection *WrappedApsConnection, username string, hashedPasswordHex string, pet string, spdBase64 string) (*WrappedTokenProvider, error) {
	return uniffiRustCallAsyncWithErrorAndResult(
		FfiConverterTypeWrappedError{}, func(status *C.RustCallStatus) *C.void {
			// rustFutureFunc
			return (*C.void)(C.uniffi_rustpushgo_fn_func_restore_token_provider(FfiConverterWrappedOSConfigINSTANCE.Lower(config), FfiConverterWrappedAPSConnectionINSTANCE.Lower(connection), rustBufferToC(FfiConverterStringINSTANCE.Lower(username)), rustBufferToC(FfiConverterStringINSTANCE.Lower(hashedPasswordHex)), rustBufferToC(FfiConverterStringINSTANCE.Lower(pet)), rustBufferToC(FfiConverterStringINSTANCE.Lower(spdBase64)),
				status,
			))
		},
		func(handle *C.void, ptr unsafe.Pointer, status *C.RustCallStatus) {
			// pollFunc
			C.ffi_rustpushgo_rust_future_poll_pointer(unsafe.Pointer(handle), ptr, status)
		},
		func(handle *C.void, status *C.RustCallStatus) unsafe.Pointer {
			// completeFunc
			return C.ffi_rustpushgo_rust_future_complete_pointer(unsafe.Pointer(handle), status)
		},
		FfiConverterWrappedTokenProviderINSTANCE.Lift, func(rustFuture *C.void, status *C.RustCallStatus) {
			// freeFunc
			C.ffi_rustpushgo_rust_future_free_pointer(unsafe.Pointer(rustFuture), status)
		})
}
