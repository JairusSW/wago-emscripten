package emscripten

import (
	"context"
	"encoding/binary"
	"io"
	"path"
	"strings"

	"github.com/wago-org/wago"
)

const (
	errBadFD      = 8
	errExist      = 20
	errFault      = 21
	errInval      = 28
	errTooMany    = 33
	errIsDir      = 31
	errNoEnt      = 44
	errNotEmpty   = 55
	errNoTTY      = 59
	openWrite     = 1
	openReadWrite = 2
	openCreate    = 64
	openExclusive = 128
	openTruncate  = 512
	openAppend    = 1024
)

type memoryFile struct {
	data   []byte
	linked bool
	refs   int
}
type openFile struct {
	file   *memoryFile
	offset int64
	flags  int32
}
type fileMapping struct {
	file   *memoryFile
	offset int64
	length uint32
	prot   int32
	flags  int32
}
type fileSystem struct {
	files    map[string]*memoryFile
	dirs     map[string]bool
	fds      map[int32]*openFile
	nextFD   int32
	bytes    int64
	tempRet0 uint32
	mappings map[uint32]fileMapping
}

func (p *plugin) setTempRet0(module wago.HostModule, params, _ []uint64) {
	fs, ok := p.fileSystem(module)
	if !ok {
		return
	}
	p.mu.Lock()
	fs.tempRet0 = uint32(params[0])
	p.mu.Unlock()
}

func (p *plugin) getTempRet0(module wago.HostModule, _ []uint64, results []uint64) {
	fs, ok := p.fileSystem(module)
	if !ok {
		results[0] = 0
		return
	}
	p.mu.Lock()
	results[0] = uint64(fs.tempRet0)
	p.mu.Unlock()
}

func newFileSystem() *fileSystem {
	return &fileSystem{files: make(map[string]*memoryFile), dirs: map[string]bool{"/": true}, fds: make(map[int32]*openFile), nextFD: 3, mappings: make(map[uint32]fileMapping)}
}

func (p *plugin) fileSystem(module wago.HostModule) (*fileSystem, bool) {
	id, err := p.callers.Resolve(module)
	if err != nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fs := p.files[id]
	if fs == nil {
		fs = newFileSystem()
		p.files[id] = fs
	}
	return fs, true
}

func cString(mem []byte, ptr uint32) (string, bool) {
	if uint64(ptr) >= uint64(len(mem)) {
		return "", false
	}
	rest := mem[ptr:]
	i := 0
	for i < len(rest) && rest[i] != 0 {
		i++
	}
	if i == len(rest) {
		return "", false
	}
	return string(rest[:i]), true
}

func cleanGuestPath(value string) (string, bool) {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return "", false
	}
	value = path.Clean("/" + value)
	return value, value != "/"
}

func negErrno(results []uint64, errno int32)   { results[0] = uint64(uint32(-errno)) }
func wasiErrno(results []uint64, errno uint32) { results[0] = uint64(errno) }

func (p *plugin) syscallOpenat(module wago.HostModule, params, results []uint64) {
	name, ok := cString(module.Memory(), uint32(params[1]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	name, ok = cleanGuestPath(name)
	if !ok {
		negErrno(results, errIsDir)
		return
	}
	flags := int32(params[2])
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(fs.fds)+3 >= p.maxOpenFiles {
		negErrno(results, errTooMany)
		return
	}
	file := fs.files[name]
	if file == nil {
		if flags&openCreate == 0 {
			negErrno(results, errNoEnt)
			return
		}
		if !fs.dirs[path.Dir(name)] {
			negErrno(results, errNoEnt)
			return
		}
		file = &memoryFile{linked: true}
		fs.files[name] = file
	} else if flags&(openCreate|openExclusive) == openCreate|openExclusive {
		negErrno(results, errExist)
		return
	}
	if flags&openTruncate != 0 {
		fs.bytes -= int64(len(file.data))
		file.data = nil
	}
	fd := fs.nextFD
	fs.nextFD++
	h := &openFile{file: file, flags: flags}
	file.refs++
	if flags&openAppend != 0 {
		h.offset = int64(len(file.data))
	}
	fs.fds[fd] = h
	results[0] = uint64(uint32(fd))
}

func (p *plugin) syscallFcntl(module wago.HostModule, params, results []uint64) {
	fd, command := int32(params[0]), int32(params[1])
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[fd]
	if h == nil {
		negErrno(results, errBadFD)
		return
	}
	switch command {
	case 0: // F_DUPFD
		if len(fs.fds)+3 >= p.maxOpenFiles {
			negErrno(results, errTooMany)
			return
		}
		candidate := int32(params[2])
		if candidate < 3 {
			candidate = 3
		}
		for fs.fds[candidate] != nil {
			candidate++
		}
		clone := *h
		fs.fds[candidate] = &clone
		h.file.refs++
		results[0] = uint64(uint32(candidate))
	case 1, 2: // F_GETFD, F_SETFD
		results[0] = 0
	case 3: // F_GETFL
		results[0] = uint64(uint32(h.flags))
	case 4: // F_SETFL
		h.flags = (h.flags & 3) | (int32(params[2]) & openAppend)
		results[0] = 0
	default:
		negErrno(results, emscriptenENOSYS)
	}
}

func (p *plugin) syscallDup3(module wago.HostModule, params, results []uint64) {
	oldFD, newFD := int32(params[0]), int32(params[1])
	if oldFD == newFD || newFD < 3 {
		negErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[oldFD]
	if h == nil {
		negErrno(results, errBadFD)
		return
	}
	if replaced := fs.fds[newFD]; replaced != nil {
		replaced.file.refs--
		if !replaced.file.linked && replaced.file.refs == 0 {
			fs.bytes -= int64(len(replaced.file.data))
		}
	} else if len(fs.fds)+3 >= p.maxOpenFiles {
		negErrno(results, errTooMany)
		return
	}
	clone := *h
	clone.file.refs++
	fs.fds[newFD] = &clone
	results[0] = uint64(uint32(newFD))
}

func (p *plugin) syscallDup(module wago.HostModule, params, results []uint64) {
	p.syscallFcntl(module, []uint64{params[0], 0, 3}, results)
}

func (p *plugin) syscallFchmod(module wago.HostModule, params, results []uint64) {
	fd := int32(params[0])
	if fd >= 0 && fd <= 2 {
		results[0] = 0
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	valid := fs.fds[fd] != nil
	p.mu.Unlock()
	if !valid {
		negErrno(results, errBadFD)
		return
	}
	results[0] = 0
}

func (p *plugin) syscallFDatasync(module wago.HostModule, params, results []uint64) {
	fd := int32(params[0])
	if fd >= 0 && fd <= 2 {
		results[0] = 0
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	valid := fs.fds[fd] != nil
	p.mu.Unlock()
	if !valid {
		negErrno(results, errBadFD)
		return
	}
	results[0] = 0
}

func (p *plugin) syscallFadvise(module wago.HostModule, params, results []uint64) {
	p.syscallFDatasync(module, params, results)
}

func (p *plugin) syscallChmod(module wago.HostModule, params, results []uint64) {
	adapted := []uint64{uint64(^uint32(99)), params[0], 0, 0}
	p.syscallFaccessat(module, adapted, results)
}

func (p *plugin) syscallUtimensat(module wago.HostModule, params, results []uint64) {
	p.syscallFaccessat(module, []uint64{params[0], params[1], 0, params[3]}, results)
}

func (p *plugin) syscallReadlinkat(_ wago.HostModule, _ []uint64, results []uint64) {
	negErrno(results, errInval)
}

func (p *plugin) syscallIoctl(_ wago.HostModule, _ []uint64, results []uint64) {
	negErrno(results, errNoTTY)
}

func (p *plugin) syscallFaccessat(module wago.HostModule, params, results []uint64) {
	name, ok := cString(module.Memory(), uint32(params[1]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	name, ok = cleanGuestPath(name)
	if !ok {
		results[0] = 0
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errNoEnt)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fs.files[name] == nil && !fs.dirs[name] {
		negErrno(results, errNoEnt)
		return
	}
	results[0] = 0
}

func (p *plugin) syscallMkdirat(module wago.HostModule, params, results []uint64) {
	name, ok := cString(module.Memory(), uint32(params[1]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	name, ok = cleanGuestPath(name)
	if !ok {
		negErrno(results, errExist)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errInval)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fs.files[name] != nil || fs.dirs[name] {
		negErrno(results, errExist)
		return
	}
	if !fs.dirs[path.Dir(name)] {
		negErrno(results, errNoEnt)
		return
	}
	fs.dirs[name] = true
	results[0] = 0
}

func (p *plugin) syscallUnlinkat(module wago.HostModule, params, results []uint64) {
	name, ok := cString(module.Memory(), uint32(params[1]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	name, ok = cleanGuestPath(name)
	if !ok {
		negErrno(results, errInval)
		return
	}
	removeDir := uint32(params[2])&0x200 != 0
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errNoEnt)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if removeDir {
		if !fs.dirs[name] {
			negErrno(results, errNoEnt)
			return
		}
		prefix := name + "/"
		for child := range fs.files {
			if strings.HasPrefix(child, prefix) {
				negErrno(results, errNotEmpty)
				return
			}
		}
		for child := range fs.dirs {
			if child != name && strings.HasPrefix(child, prefix) {
				negErrno(results, errNotEmpty)
				return
			}
		}
		delete(fs.dirs, name)
		results[0] = 0
		return
	}
	file := fs.files[name]
	if file == nil {
		negErrno(results, errNoEnt)
		return
	}
	delete(fs.files, name)
	file.linked = false
	if file.refs == 0 {
		fs.bytes -= int64(len(file.data))
	}
	results[0] = 0
}

func (p *plugin) syscallRenameat(module wago.HostModule, params, results []uint64) {
	oldName, ok := cString(module.Memory(), uint32(params[1]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	newName, ok := cString(module.Memory(), uint32(params[3]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	oldName, oldOK := cleanGuestPath(oldName)
	newName, newOK := cleanGuestPath(newName)
	if !oldOK || !newOK {
		negErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errNoEnt)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !fs.dirs[path.Dir(newName)] {
		negErrno(results, errNoEnt)
		return
	}
	if file := fs.files[oldName]; file != nil {
		if replaced := fs.files[newName]; replaced != nil {
			replaced.linked = false
			if replaced.refs == 0 {
				fs.bytes -= int64(len(replaced.data))
			}
		}
		fs.files[newName] = file
		delete(fs.files, oldName)
		results[0] = 0
		return
	}
	if fs.dirs[oldName] {
		prefix := oldName + "/"
		for child := range fs.files {
			if strings.HasPrefix(child, prefix) {
				negErrno(results, errNotEmpty)
				return
			}
		}
		for child := range fs.dirs {
			if child != oldName && strings.HasPrefix(child, prefix) {
				negErrno(results, errNotEmpty)
				return
			}
		}
		fs.dirs[newName] = true
		delete(fs.dirs, oldName)
		results[0] = 0
		return
	}
	negErrno(results, errNoEnt)
}

func (p *plugin) syscallGetcwd(module wago.HostModule, params, results []uint64) {
	mem, ptr, size := module.Memory(), uint32(params[0]), uint32(params[1])
	if size < 2 || uint64(ptr)+2 > uint64(len(mem)) {
		negErrno(results, errInval)
		return
	}
	mem[ptr], mem[ptr+1] = '/', 0
	results[0] = 2
}

func (p *plugin) syscallFtruncate(module wago.HostModule, params, results []uint64) {
	fd, size := int32(params[0]), int64(params[1])
	if size < 0 || size > p.maxFilesystemBytes {
		negErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[fd]
	if h == nil {
		negErrno(results, errBadFD)
		return
	}
	change := size - int64(len(h.file.data))
	if fs.bytes+change > p.maxFilesystemBytes {
		negErrno(results, errInval)
		return
	}
	if change < 0 {
		h.file.data = h.file.data[:size]
	} else if change > 0 {
		h.file.data = append(h.file.data, make([]byte, int(change))...)
	}
	fs.bytes += change
	results[0] = 0
}

func (p *plugin) syscallTruncate(module wago.HostModule, params, results []uint64) {
	name, ok := cString(module.Memory(), uint32(params[0]))
	if !ok {
		negErrno(results, errFault)
		return
	}
	name, ok = cleanGuestPath(name)
	if !ok {
		negErrno(results, errIsDir)
		return
	}
	size := int64(params[1])
	if size < 0 || size > p.maxFilesystemBytes {
		negErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errNoEnt)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	file := fs.files[name]
	if file == nil {
		negErrno(results, errNoEnt)
		return
	}
	change := size - int64(len(file.data))
	if fs.bytes+change > p.maxFilesystemBytes {
		negErrno(results, errInval)
		return
	}
	if change < 0 {
		file.data = file.data[:size]
	} else if change > 0 {
		file.data = append(file.data, make([]byte, int(change))...)
	}
	fs.bytes += change
	results[0] = 0
}

func (p *plugin) syscallFallocate(module wago.HostModule, params, results []uint64) {
	fd, mode, offset, length := int32(params[0]), int32(params[1]), int64(params[2]), int64(params[3])
	if mode != 0 {
		negErrno(results, emscriptenENOSYS)
		return
	}
	if offset < 0 || length < 0 || offset > p.maxFilesystemBytes-length {
		negErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	h := fs.fds[fd]
	size := int64(0)
	if h != nil {
		size = int64(len(h.file.data))
	}
	p.mu.Unlock()
	if h == nil {
		negErrno(results, errBadFD)
		return
	}
	if end := offset + length; end > size {
		p.syscallFtruncate(module, []uint64{uint64(uint32(fd)), uint64(end)}, results)
		return
	}
	results[0] = 0
}

func writeStat(mem []byte, ptr uint32, mode uint32, size int64) bool {
	if uint64(ptr)+96 > uint64(len(mem)) {
		return false
	}
	clear(mem[ptr : ptr+96])
	binary.LittleEndian.PutUint32(mem[ptr:], 1)
	binary.LittleEndian.PutUint32(mem[ptr+4:], mode)
	binary.LittleEndian.PutUint32(mem[ptr+8:], 1)
	binary.LittleEndian.PutUint64(mem[ptr+24:], uint64(size))
	binary.LittleEndian.PutUint32(mem[ptr+32:], 4096)
	binary.LittleEndian.PutUint32(mem[ptr+36:], uint32((size+511)/512))
	binary.LittleEndian.PutUint64(mem[ptr+88:], 1)
	return true
}

func (p *plugin) statPath(module wago.HostModule, pathPtr, statPtr uint32, results []uint64) {
	name, ok := cString(module.Memory(), pathPtr)
	if !ok || name == "" {
		negErrno(results, errFault)
		return
	}
	name = path.Clean("/" + name)
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errNoEnt)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mode, size := uint32(0), int64(0)
	if file := fs.files[name]; file != nil {
		mode, size = 0100644, int64(len(file.data))
	} else if fs.dirs[name] {
		mode = 0040755
	} else {
		negErrno(results, errNoEnt)
		return
	}
	if !writeStat(module.Memory(), statPtr, mode, size) {
		negErrno(results, errFault)
		return
	}
	results[0] = 0
}

func (p *plugin) syscallStat(module wago.HostModule, params, results []uint64) {
	p.statPath(module, uint32(params[0]), uint32(params[1]), results)
}

func (p *plugin) syscallNewfstatat(module wago.HostModule, params, results []uint64) {
	p.statPath(module, uint32(params[1]), uint32(params[2]), results)
}

func (p *plugin) syscallFstat(module wago.HostModule, params, results []uint64) {
	fd, statPtr := int32(params[0]), uint32(params[1])
	if fd >= 0 && fd <= 2 {
		if !writeStat(module.Memory(), statPtr, 0020666, 0) {
			negErrno(results, errFault)
			return
		}
		results[0] = 0
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		negErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[fd]
	if h == nil {
		negErrno(results, errBadFD)
		return
	}
	if !writeStat(module.Memory(), statPtr, 0100644, int64(len(h.file.data))) {
		negErrno(results, errFault)
		return
	}
	results[0] = 0
}

func guestIOVecs(mem []byte, ptr, count uint32) ([][]byte, bool) {
	if uint64(ptr)+uint64(count)*8 > uint64(len(mem)) {
		return nil, false
	}
	vectors := make([][]byte, 0, count)
	for i := uint32(0); i < count; i++ {
		at := ptr + i*8
		base, size := binary.LittleEndian.Uint32(mem[at:]), binary.LittleEndian.Uint32(mem[at+4:])
		if uint64(base)+uint64(size) > uint64(len(mem)) {
			return nil, false
		}
		vectors = append(vectors, mem[base:base+size])
	}
	return vectors, true
}

func (p *plugin) wasiFDWrite(module wago.HostModule, params, results []uint64) {
	fd, ptr, count, writtenPtr := int32(params[0]), uint32(params[1]), uint32(params[2]), uint32(params[3])
	mem := module.Memory()
	vectors, ok := guestIOVecs(mem, ptr, count)
	if !ok || uint64(writtenPtr)+4 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	var writer io.Writer
	if fd == 1 {
		writer = p.stdout
	} else if fd == 2 {
		writer = p.stderr
	}
	var fs *fileSystem
	var h *openFile
	if writer == nil {
		fs, ok = p.fileSystem(module)
		if ok {
			p.mu.Lock()
			h = fs.fds[fd]
		}
		if !ok || h == nil {
			if ok {
				p.mu.Unlock()
			}
			wasiErrno(results, errBadFD)
			return
		}
		defer p.mu.Unlock()
		if h.flags&3 == 0 {
			wasiErrno(results, errBadFD)
			return
		}
	}
	var total uint32
	for _, vector := range vectors {
		if writer != nil {
			n, err := writer.Write(vector)
			total += uint32(n)
			if err != nil || n != len(vector) {
				wasiErrno(results, errInval)
				return
			}
			continue
		}
		end := h.offset + int64(len(vector))
		if h.flags&openAppend != 0 {
			h.offset = int64(len(h.file.data))
			end = h.offset + int64(len(vector))
		}
		if end < 0 || end > p.maxFilesystemBytes || fs.bytes+end-int64(len(h.file.data)) > p.maxFilesystemBytes {
			wasiErrno(results, errInval)
			return
		}
		if end > int64(len(h.file.data)) {
			old := len(h.file.data)
			h.file.data = append(h.file.data, make([]byte, int(end)-old)...)
			fs.bytes += int64(len(h.file.data) - old)
		}
		copy(h.file.data[h.offset:end], vector)
		h.offset = end
		total += uint32(len(vector))
	}
	binary.LittleEndian.PutUint32(mem[writtenPtr:], total)
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDRead(module wago.HostModule, params, results []uint64) {
	fd, ptr, count, readPtr := int32(params[0]), uint32(params[1]), uint32(params[2]), uint32(params[3])
	mem := module.Memory()
	vectors, ok := guestIOVecs(mem, ptr, count)
	if !ok || uint64(readPtr)+4 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	var reader io.Reader
	if fd == 0 {
		reader = p.stdin
	}
	var h *openFile
	if reader == nil {
		fs, found := p.fileSystem(module)
		if found {
			p.mu.Lock()
			h = fs.fds[fd]
		}
		if !found || h == nil {
			if found {
				p.mu.Unlock()
			}
			wasiErrno(results, errBadFD)
			return
		}
		defer p.mu.Unlock()
		if h.flags&3 == openWrite {
			wasiErrno(results, errBadFD)
			return
		}
	}
	var total uint32
	for _, vector := range vectors {
		var n int
		if reader != nil {
			n, _ = reader.Read(vector)
		} else if h.offset < int64(len(h.file.data)) {
			n = copy(vector, h.file.data[h.offset:])
			h.offset += int64(n)
		}
		total += uint32(n)
		if n < len(vector) {
			break
		}
	}
	binary.LittleEndian.PutUint32(mem[readPtr:], total)
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDClose(module wago.HostModule, params, results []uint64) {
	fd := int32(params[0])
	if fd >= 0 && fd <= 2 {
		wasiErrno(results, 0)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fs.fds[fd] == nil {
		wasiErrno(results, errBadFD)
		return
	}
	file := fs.fds[fd].file
	delete(fs.fds, fd)
	file.refs--
	if !file.linked && file.refs == 0 {
		fs.bytes -= int64(len(file.data))
	}
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDSeek(module wago.HostModule, params, results []uint64) {
	fd, delta, whence, resultPtr := int32(params[0]), int64(params[1]), uint32(params[2]), uint32(params[3])
	mem := module.Memory()
	if uint64(resultPtr)+8 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[fd]
	if h == nil {
		wasiErrno(results, errBadFD)
		return
	}
	var base int64
	switch whence {
	case 0:
		base = 0
	case 1:
		base = h.offset
	case 2:
		base = int64(len(h.file.data))
	default:
		wasiErrno(results, errInval)
		return
	}
	if delta < -base {
		wasiErrno(results, errInval)
		return
	}
	h.offset = base + delta
	binary.LittleEndian.PutUint64(mem[resultPtr:], uint64(h.offset))
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDStatGet(module wago.HostModule, params, results []uint64) {
	fd, ptr := int32(params[0]), uint32(params[1])
	mem := module.Memory()
	if uint64(ptr)+24 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	fileType := byte(2)
	if fd > 2 {
		fs, ok := p.fileSystem(module)
		if !ok {
			wasiErrno(results, errBadFD)
			return
		}
		p.mu.Lock()
		h := fs.fds[fd]
		p.mu.Unlock()
		if h == nil {
			wasiErrno(results, errBadFD)
			return
		}
		fileType = 4
	}
	clear(mem[ptr : ptr+24])
	mem[ptr] = fileType
	binary.LittleEndian.PutUint64(mem[ptr+8:], ^uint64(0))
	binary.LittleEndian.PutUint64(mem[ptr+16:], ^uint64(0))
	wasiErrno(results, 0)
}

func (p *plugin) wasiFileStatGet(module wago.HostModule, params, results []uint64) {
	fd, ptr := int32(params[0]), uint32(params[1])
	mem := module.Memory()
	if uint64(ptr)+64 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	fileType, size := byte(2), int64(0)
	if fd > 2 {
		fs, ok := p.fileSystem(module)
		if !ok {
			wasiErrno(results, errBadFD)
			return
		}
		p.mu.Lock()
		h := fs.fds[fd]
		if h != nil {
			size = int64(len(h.file.data))
		}
		p.mu.Unlock()
		if h == nil {
			wasiErrno(results, errBadFD)
			return
		}
		fileType = 4
	}
	clear(mem[ptr : ptr+64])
	binary.LittleEndian.PutUint64(mem[ptr:], 1)
	binary.LittleEndian.PutUint64(mem[ptr+8:], 1)
	mem[ptr+16] = fileType
	binary.LittleEndian.PutUint64(mem[ptr+24:], 1)
	binary.LittleEndian.PutUint64(mem[ptr+32:], uint64(size))
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDSync(module wago.HostModule, params, results []uint64) {
	fd := int32(params[0])
	if fd >= 0 && fd <= 2 {
		wasiErrno(results, 0)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	valid := fs.fds[fd] != nil
	p.mu.Unlock()
	if !valid {
		wasiErrno(results, errBadFD)
		return
	}
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDSetFlags(module wago.HostModule, params, results []uint64) {
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := fs.fds[int32(params[0])]
	if h == nil {
		wasiErrno(results, errBadFD)
		return
	}
	if uint32(params[1])&1 != 0 {
		h.flags |= openAppend
	} else {
		h.flags &^= openAppend
	}
	wasiErrno(results, 0)
}

func (p *plugin) wasiFDSetSize(module wago.HostModule, params, results []uint64) {
	var syscallResult [1]uint64
	p.syscallFtruncate(module, params, syscallResult[:])
	code := int32(uint32(syscallResult[0]))
	if code < 0 {
		wasiErrno(results, uint32(-code))
	} else {
		wasiErrno(results, 0)
	}
}

func (p *plugin) wasiPositionedIO(write bool) wago.HostFunc {
	return func(module wago.HostModule, params, results []uint64) {
		fd, offset := int32(params[0]), int64(params[3])
		fs, ok := p.fileSystem(module)
		if !ok {
			wasiErrno(results, errBadFD)
			return
		}
		p.mu.Lock()
		h := fs.fds[fd]
		if h == nil || offset < 0 {
			p.mu.Unlock()
			wasiErrno(results, errInval)
			return
		}
		saved := h.offset
		h.offset = offset
		p.mu.Unlock()
		adapted := []uint64{params[0], params[1], params[2], params[4]}
		if write {
			p.wasiFDWrite(module, adapted, results)
		} else {
			p.wasiFDRead(module, adapted, results)
		}
		p.mu.Lock()
		h.offset = saved
		p.mu.Unlock()
	}
}

func (p *plugin) wasiFDAdvise(module wago.HostModule, params, results []uint64) {
	p.wasiFDSync(module, params, results)
}

func (p *plugin) wasiFDAllocate(module wago.HostModule, params, results []uint64) {
	fd, offset, length := int32(params[0]), int64(params[1]), int64(params[2])
	if offset < 0 || length < 0 || offset > p.maxFilesystemBytes-length {
		wasiErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	size, valid := int64(0), false
	if h := fs.fds[fd]; h != nil {
		size, valid = int64(len(h.file.data)), true
	}
	p.mu.Unlock()
	if !valid {
		wasiErrno(results, errBadFD)
		return
	}
	if end := offset + length; end > size {
		p.wasiFDSetSize(module, []uint64{uint64(uint32(fd)), uint64(end)}, results)
		return
	}
	wasiErrno(results, 0)
}

func (p *plugin) mmap(module wago.HostModule, length uint32, prot, flags, fd int32, offset int64, allocatedPtr, addressPtr uint32, results []uint64) {
	mem := module.Memory()
	if length == 0 || int64(length) > p.maxFilesystemBytes || offset < 0 || uint64(allocatedPtr)+4 > uint64(len(mem)) || uint64(addressPtr)+4 > uint64(len(mem)) {
		wasiErrno(results, errInval)
		return
	}
	fs, ok := p.fileSystem(module)
	if !ok {
		wasiErrno(results, errBadFD)
		return
	}
	p.mu.Lock()
	h := fs.fds[fd]
	if h == nil {
		p.mu.Unlock()
		wasiErrno(results, errBadFD)
		return
	}
	file := h.file
	p.mu.Unlock()
	allocation, err := p.invoker.Invoke(context.Background(), module, "emscripten_builtin_memalign", 65536, uint64(length))
	if err != nil || len(allocation) != 1 || uint32(allocation[0]) == 0 {
		wasiErrno(results, errInval)
		return
	}
	address := uint32(allocation[0])
	mem = module.Memory()
	if uint64(address)+uint64(length) > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	clear(mem[address : address+length])
	p.mu.Lock()
	if offset < int64(len(file.data)) {
		copy(mem[address:address+length], file.data[offset:])
	}
	file.refs++
	fs.mappings[address] = fileMapping{file: file, offset: offset, length: length, prot: prot, flags: flags}
	p.mu.Unlock()
	binary.LittleEndian.PutUint32(mem[allocatedPtr:], 1)
	binary.LittleEndian.PutUint32(mem[addressPtr:], address)
	wasiErrno(results, 0)
}

func (p *plugin) mmap32(module wago.HostModule, params, results []uint64) {
	p.mmap(module, uint32(params[0]), int32(params[1]), int32(params[2]), int32(params[3]), int64(int32(params[4])), uint32(params[5]), uint32(params[6]), results)
}

func (p *plugin) mmap64(module wago.HostModule, params, results []uint64) {
	p.mmap(module, uint32(params[0]), int32(params[1]), int32(params[2]), int32(params[3]), int64(params[4]), uint32(params[5]), uint32(params[6]), results)
}

func (p *plugin) munmap(module wago.HostModule, params, results []uint64) {
	address := uint32(params[0])
	fs, ok := p.fileSystem(module)
	if !ok {
		results[0] = 0
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mapping, ok := fs.mappings[address]
	if !ok {
		results[0] = 0
		return
	}
	delete(fs.mappings, address)
	if mapping.prot&2 != 0 && mapping.flags&2 == 0 {
		end := mapping.offset + int64(mapping.length)
		growth := end - int64(len(mapping.file.data))
		if growth < 0 {
			growth = 0
		}
		if end <= p.maxFilesystemBytes && fs.bytes+growth <= p.maxFilesystemBytes {
			if end > int64(len(mapping.file.data)) {
				old := len(mapping.file.data)
				mapping.file.data = append(mapping.file.data, make([]byte, int(end)-old)...)
				fs.bytes += int64(len(mapping.file.data) - old)
			}
			mem := module.Memory()
			if uint64(address)+uint64(mapping.length) <= uint64(len(mem)) {
				copy(mapping.file.data[mapping.offset:end], mem[address:address+mapping.length])
			}
		}
	}
	mapping.file.refs--
	if !mapping.file.linked && mapping.file.refs == 0 {
		fs.bytes -= int64(len(mapping.file.data))
	}
	results[0] = 0
}

func (p *plugin) wasiEnvironSizesGet(module wago.HostModule, params, results []uint64) {
	mem := module.Memory()
	countPtr, bytesPtr := uint32(params[0]), uint32(params[1])
	if uint64(countPtr)+4 > uint64(len(mem)) || uint64(bytesPtr)+4 > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, entry := range p.env {
		total += len(entry) + 1
	}
	binary.LittleEndian.PutUint32(mem[countPtr:], uint32(len(p.env)))
	binary.LittleEndian.PutUint32(mem[bytesPtr:], uint32(total))
	wasiErrno(results, 0)
}

func (p *plugin) wasiEnvironGet(module wago.HostModule, params, results []uint64) {
	mem := module.Memory()
	pointers, data := uint32(params[0]), uint32(params[1])
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, entry := range p.env {
		total += len(entry) + 1
	}
	if uint64(pointers)+uint64(len(p.env))*4 > uint64(len(mem)) || uint64(data)+uint64(total) > uint64(len(mem)) {
		wasiErrno(results, errFault)
		return
	}
	offset := data
	for i, entry := range p.env {
		binary.LittleEndian.PutUint32(mem[pointers+uint32(i*4):], offset)
		copy(mem[offset:], entry)
		offset += uint32(len(entry))
		mem[offset] = 0
		offset++
	}
	wasiErrno(results, 0)
}
