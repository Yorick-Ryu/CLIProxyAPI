use flate2::{Compress, CompressError, Compression, FlushCompress, Status};
use std::{ptr, slice};

const OUTPUT_RESERVE: usize = 4096;
const SYNC_TRAILER: &[u8] = &[0, 0, 255, 255];

pub struct CpaDeflate {
    compressor: Option<Compress>,
    output: Vec<u8>,
    has_input: bool,
}

#[repr(C)]
pub struct CpaDeflateOutput {
    data: *const u8,
    len: usize,
}

#[unsafe(no_mangle)]
extern "C" fn cpa_deflate_new(bits: u8, level: u8) -> *mut CpaDeflate {
    if !(9..=15).contains(&bits) || level > 9 {
        return ptr::null_mut();
    }
    Box::into_raw(Box::new(CpaDeflate {
        compressor: Some(Compress::new_with_window_bits(
            Compression::new(u32::from(level)),
            false,
            bits,
        )),
        output: Vec::new(),
        has_input: false,
    }))
}

#[unsafe(no_mangle)]
unsafe extern "C" fn cpa_deflate_free(encoder: *mut CpaDeflate) {
    // SAFETY: the caller exclusively owns a live handle returned by cpa_deflate_new.
    drop(unsafe { Box::from_raw(encoder) });
}

#[unsafe(no_mangle)]
unsafe extern "C" fn cpa_deflate_write(
    encoder: *mut CpaDeflate,
    input: *const u8,
    len: usize,
) -> bool {
    // SAFETY: the caller serializes all operations on a live encoder.
    let Some(encoder) = (unsafe { encoder.as_mut() }) else {
        return false;
    };
    // SAFETY: the caller lends len initialized, nonempty bytes for this call.
    let data = unsafe { slice::from_raw_parts(input, len) };
    if !encoder.has_input {
        encoder.output = Vec::new();
        encoder.has_input = true;
    }
    let Some(compressor) = &mut encoder.compressor else {
        return false;
    };
    if !encode(compressor, data, &mut encoder.output) {
        encoder.compressor = None;
        return false;
    }
    true
}

#[unsafe(no_mangle)]
unsafe extern "C" fn cpa_deflate_flush(encoder: *mut CpaDeflate) -> CpaDeflateOutput {
    let failed = CpaDeflateOutput {
        data: ptr::null(),
        len: 0,
    };
    // SAFETY: the caller serializes all operations on a live encoder.
    let Some(encoder) = (unsafe { encoder.as_mut() }) else {
        return failed;
    };
    let Some(compressor) = &mut encoder.compressor else {
        return failed;
    };
    if std::mem::take(&mut encoder.has_input) {
        if flush(compressor, &mut encoder.output).is_err() {
            return failed;
        }
    } else {
        encoder.output = vec![0, 0, 0, 255, 255];
    }
    if !encoder.output.ends_with(SYNC_TRAILER) {
        return failed;
    }
    // The caller reads this buffer before unlocking the handle.
    CpaDeflateOutput {
        data: encoder.output.as_ptr(),
        len: encoder.output.len(),
    }
}

// Match the raw DEFLATE fragment loop used by the Codex tungstenite fork.
// The Go frame writer removes SYNC_TRAILER from final fragments.
fn encode(compressor: &mut Compress, mut data: &[u8], output: &mut Vec<u8>) -> bool {
    loop {
        output.reserve(OUTPUT_RESERVE);
        let before = compressor.total_in();
        // The backend records its last input pointer. Its final call must use
        // a Rust-owned empty slice before returning the borrowed Go buffer.
        let input = if data.is_empty() { &[] } else { data };
        let Ok(status) = compressor.compress_vec(input, output, FlushCompress::None) else {
            return false;
        };
        let read = (compressor.total_in() - before) as usize;
        data = &data[read..];
        match status {
            Status::StreamEnd => return false,
            Status::BufError if read == 0 => return input.is_empty(),
            Status::Ok | Status::BufError => {}
        }
    }
}

fn flush(compressor: &mut Compress, output: &mut Vec<u8>) -> Result<(), CompressError> {
    loop {
        output.reserve(OUTPUT_RESERVE);
        let before = compressor.total_out();
        compressor.compress_vec(&[], output, FlushCompress::Sync)?;
        if before == compressor.total_out() {
            break;
        }
    }
    Ok(())
}
