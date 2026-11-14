//! Printing, the desktop's `window.print()` (Onboarding.tsx's backup codes, Jonas 2026-11-14: Print, like the web).
//!
//! GPUI has no print API. On Linux the app hands the system's print dialog a document through the XDG desktop portal
//! (`org.freedesktop.portal.Print`, which GNOME, KDE and the other desktops implement with their own dialog): a
//! `PreparePrint` shows the dialog, and a `Print` with the token it answered passes the document as a file descriptor.
//!
//! The document is a small PDF the app writes itself ([`codes_pdf`]: the portal's backends print PDF; plain text is not
//! promised). It never touches the disk: it lives in an anonymous memory file (`memfd_create`) sealed against changes,
//! which the kernel frees when the last descriptor closes (the app's when the print request has answered, the
//! backend's when it has read the job). Every buffer of it on the app's side is a [`Zeroizing`] one. What the printing
//! system then does with the job (a spool file, the printer) is the system's, as for the web's print.
//!
//! On macOS the same PDF is printed with AppKit's print operation (`NSPrintOperation`, the system's print panel): an
//! `NSImage` reads the document from an `NSData` laid over the app's own zeroizing buffer (no copy, nothing on disk),
//! an `NSImageView` of the page's size draws it, and the operation runs on the main thread, as AppKit requires.
//!
//! Windows has no system PDF printing the app can hand a document to without a file, so there the app draws the same
//! document itself: the system's print dialog (`PrintDlgW`) answers a printer's device context, and each text run of
//! the PDF's content stream ([`runs`]) is drawn on it with GDI at its place on the page, in the faces the PDF names
//! (Arial for Helvetica, Courier New for Courier). Nothing is written to the disk there either.
//!
//! Any other platform answers [`PrintError::Unsupported`].

use std::fmt::Write as _;

use zeroize::Zeroizing;

/// What a print request came to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Printed {
    /// The dialog took the document.
    Sent,
    /// The person closed the dialog without printing.
    Cancelled,
}

/// Why nothing was printed.
#[derive(Debug)]
pub enum PrintError {
    /// This platform has no print path (or no print portal runs).
    Unsupported,
    /// The document could not be handed over.
    Failed(String),
}

impl PrintError {
    /// The code a refusal shows (`factor.error.other`'s `{code}`).
    pub fn code(&self) -> &'static str {
        match self {
            Self::Unsupported => "E_PRINT_UNSUPPORTED",
            Self::Failed(_) => "E_PRINT_FAILED",
        }
    }
}

/// A4 in PostScript points, 56pt (about 2 cm) margins.
const PAGE_W: f32 = 595.0;
const PAGE_H: f32 = 842.0;
const MARGIN: f32 = 56.0;
/// The title, the copy and the codes: their sizes and line advances, in points.
const TITLE_PT: f32 = 16.0;
const BODY_PT: f32 = 11.0;
const CODE_PT: f32 = 12.0;
const LEAD: f32 = 1.45;
/// The copy is wrapped at this many characters (Helvetica at 11pt across the text width, conservatively).
const WRAP: usize = 80;

/// A PDF of the backup codes as the web prints them (CodeList.tsx under `@media print`): the title, the copy, the list's
/// label and the codes, numbered, one per line, in a monospaced face. Every buffer it is built in is zeroized.
pub fn codes_pdf(title: &str, body: &str, label: &str, codes: &[String]) -> Zeroizing<Vec<u8>> {
    let mut text = Zeroizing::new(String::new());
    let mut y = PAGE_H - MARGIN - TITLE_PT;
    let put = |text: &mut String, font: &str, size: f32, y: f32, line: &str| {
        let _ = writeln!(
            text,
            "BT /{font} {size} Tf {MARGIN} {y:.2} Td ({}) Tj ET",
            pdf_string(line).as_str()
        );
    };
    put(&mut text, "F2", TITLE_PT, y, title);
    y -= TITLE_PT * LEAD * 1.5;
    for line in wrap(body, WRAP) {
        put(&mut text, "F1", BODY_PT, y, &line);
        y -= BODY_PT * LEAD;
    }
    y -= BODY_PT * LEAD;
    put(&mut text, "F2", BODY_PT, y, label);
    y -= CODE_PT * LEAD * 1.25;
    for (i, code) in codes.iter().enumerate() {
        let numbered = Zeroizing::new(format!("{:>2}. {}", i + 1, code));
        put(&mut text, "F3", CODE_PT, y, &numbered);
        y -= CODE_PT * LEAD;
    }
    document(text.as_bytes())
}

/// Lays one content stream out as a one-page PDF: the catalog, the page tree, the page, its content and the three
/// standard faces (Helvetica, Helvetica-Bold, Courier; WinAnsi), then the cross-reference table and the trailer.
fn document(content: &[u8]) -> Zeroizing<Vec<u8>> {
    let mut out = Zeroizing::new(Vec::with_capacity(content.len() + 1024));
    let mut offsets = Vec::with_capacity(7);
    out.extend_from_slice(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n");
    let font = |name: &str| {
        format!("<< /Type /Font /Subtype /Type1 /BaseFont /{name} /Encoding /WinAnsiEncoding >>")
    };
    let objects: [Vec<u8>; 3] = [
        b"<< /Type /Catalog /Pages 2 0 R >>".to_vec(),
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>".to_vec(),
        format!(
            "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 {PAGE_W} {PAGE_H}] \
             /Resources << /Font << /F1 5 0 R /F2 6 0 R /F3 7 0 R >> >> /Contents 4 0 R >>"
        )
        .into_bytes(),
    ];
    for (i, body) in objects.iter().enumerate() {
        offsets.push(out.len());
        out.extend_from_slice(format!("{} 0 obj\n", i + 1).as_bytes());
        out.extend_from_slice(body);
        out.extend_from_slice(b"\nendobj\n");
    }
    offsets.push(out.len());
    out.extend_from_slice(format!("4 0 obj\n<< /Length {} >>\nstream\n", content.len()).as_bytes());
    out.extend_from_slice(content);
    out.extend_from_slice(b"\nendstream\nendobj\n");
    for (n, name) in [(5, "Helvetica"), (6, "Helvetica-Bold"), (7, "Courier")] {
        offsets.push(out.len());
        out.extend_from_slice(format!("{n} 0 obj\n{}\nendobj\n", font(name)).as_bytes());
    }
    let xref = out.len();
    out.extend_from_slice(
        format!("xref\n0 {}\n0000000000 65535 f \n", offsets.len() + 1).as_bytes(),
    );
    for at in &offsets {
        out.extend_from_slice(format!("{at:010} 00000 n \n").as_bytes());
    }
    out.extend_from_slice(
        format!(
            "trailer\n<< /Size {} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n",
            offsets.len() + 1
        )
        .as_bytes(),
    );
    out
}

/// A PDF literal string (WinAnsi): `\`, `(` and `)` escaped, the typographic quotes and dashes the copy uses in their
/// WinAnsi codes, anything else outside it as `?`.
fn pdf_string(s: &str) -> Zeroizing<String> {
    let mut out = Zeroizing::new(String::with_capacity(s.len() + 8));
    for c in s.chars() {
        let byte: Option<u8> = match c {
            '\\' | '(' | ')' => {
                out.push('\\');
                out.push(c);
                continue;
            }
            ' '..='~' => {
                out.push(c);
                continue;
            }
            '‘' => Some(0x91),
            '’' => Some(0x92),
            '“' => Some(0x93),
            '”' => Some(0x94),
            '–' => Some(0x96),
            '—' => Some(0x97),
            '…' => Some(0x85),
            c if ('\u{a0}'..='\u{ff}').contains(&c) => u8::try_from(u32::from(c)).ok(),
            _ => None,
        };
        match byte {
            Some(b) => {
                let _ = write!(out, "\\{b:03o}");
            }
            None => out.push('?'),
        }
    }
    out
}

/// Words joined into lines of at most `width` characters (a longer word stands alone).
fn wrap(s: &str, width: usize) -> Vec<String> {
    let mut lines = Vec::new();
    let mut line = String::new();
    for word in s.split_whitespace() {
        if !line.is_empty() && line.chars().count() + 1 + word.chars().count() > width {
            lines.push(std::mem::take(&mut line));
        }
        if !line.is_empty() {
            line.push(' ');
        }
        line.push_str(word);
    }
    if !line.is_empty() {
        lines.push(line);
    }
    lines
}

/// Prints `pdf` through the system's print dialog, titled `title` (see the module's comment): on Linux, the XDG portal.
#[cfg(target_os = "linux")]
pub async fn system(title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    use std::io::{Seek as _, SeekFrom, Write as _};
    use std::os::fd::AsFd as _;

    use ashpd::desktop::ResponseError;
    use ashpd::desktop::print::{PreparePrintOptions, PrintOptions, PrintProxy};
    use rustix::fs::{MemfdFlags, SealFlags};

    let failed = |e: &dyn std::fmt::Display| PrintError::Failed(e.to_string());
    let fd = rustix::fs::memfd_create(
        "dilla-print",
        MemfdFlags::CLOEXEC | MemfdFlags::ALLOW_SEALING,
    )
    .map_err(|e| failed(&e))?;
    let mut file = std::fs::File::from(fd);
    file.write_all(&pdf).map_err(|e| failed(&e))?;
    drop(pdf);
    file.seek(SeekFrom::Start(0)).map_err(|e| failed(&e))?;
    // The backend reads what the app wrote, and nothing can change it after.
    rustix::fs::fcntl_add_seals(
        &file,
        SealFlags::SEAL | SealFlags::SHRINK | SealFlags::GROW | SealFlags::WRITE,
    )
    .map_err(|e| failed(&e))?;
    let proxy = PrintProxy::new()
        .await
        .map_err(|_| PrintError::Unsupported)?;
    let prepared = proxy
        .prepare_print(
            None,
            &title,
            Default::default(),
            Default::default(),
            PreparePrintOptions::default().set_modal(true),
        )
        .await
        .map_err(|e| failed(&e))?
        .response();
    let prepared = match prepared {
        Ok(p) => p,
        Err(ashpd::Error::Response(ResponseError::Cancelled)) => return Ok(Printed::Cancelled),
        Err(e) => return Err(failed(&e)),
    };
    let sent = proxy
        .print(
            None,
            &title,
            &file.as_fd(),
            PrintOptions::default()
                .set_token(prepared.token)
                .set_modal(true),
        )
        .await
        .map_err(|e| failed(&e))?
        .response();
    // The app's descriptor closes here; the memory file goes when the backend has closed its own.
    drop(file);
    match sent {
        Ok(()) => Ok(Printed::Sent),
        Err(ashpd::Error::Response(ResponseError::Cancelled)) => Ok(Printed::Cancelled),
        Err(e) => Err(failed(&e)),
    }
}

/// Prints `pdf` through the system's print panel (see the module's comment): AppKit's print operation on the main
/// thread, which this (background) task waits for.
#[cfg(target_os = "macos")]
#[allow(clippy::unused_async)]
pub async fn system(title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    dispatch2::run_on_main(move |mtm| mac::print(mtm, &title, &pdf))
}

#[cfg(target_os = "macos")]
mod mac {
    use std::ffi::c_void;
    use std::ptr::NonNull;

    use objc2::rc::autoreleasepool;
    use objc2::{AnyThread as _, MainThreadMarker};
    use objc2_app_kit::{
        NSImage, NSImageView, NSPrintInfo, NSPrintOperation, NSPrintingPaginationMode,
    };
    use objc2_foundation::{NSData, NSPoint, NSRect, NSSize, NSString};

    use super::{PAGE_H, PAGE_W, PrintError, Printed};

    pub(super) fn print(
        mtm: MainThreadMarker,
        title: &str,
        pdf: &[u8],
    ) -> Result<Printed, PrintError> {
        let Some(bytes) = NonNull::new(pdf.as_ptr().cast_mut().cast::<c_void>()) else {
            return Err(PrintError::Failed("an empty document".into()));
        };
        // Every object goes with the pool, before the caller's buffer is zeroized and freed.
        autoreleasepool(|_| {
            // SAFETY: the data is read only, and the buffer outlives it and every object made from it (the pool ends
            // first); `freeWhenDone: false` leaves the buffer to its zeroizing owner.
            let data =
                unsafe { NSData::dataWithBytesNoCopy_length_freeWhenDone(bytes, pdf.len(), false) };
            let image = NSImage::initWithData(NSImage::alloc(), &data).ok_or_else(|| {
                PrintError::Failed("the document is not a PDF AppKit reads".into())
            })?;
            let page = NSRect::new(
                NSPoint::new(0.0, 0.0),
                NSSize::new(f64::from(PAGE_W), f64::from(PAGE_H)),
            );
            let view = NSImageView::initWithFrame(mtm.alloc(), page);
            view.setImage(Some(&image));
            // The page is the document's own A4 with its own margins: the view fits the paper, centred, edge to edge.
            let info = NSPrintInfo::new();
            info.setTopMargin(0.0);
            info.setBottomMargin(0.0);
            info.setLeftMargin(0.0);
            info.setRightMargin(0.0);
            info.setHorizontallyCentered(true);
            info.setVerticallyCentered(true);
            info.setHorizontalPagination(NSPrintingPaginationMode::Fit);
            info.setVerticalPagination(NSPrintingPaginationMode::Fit);
            let operation = NSPrintOperation::printOperationWithView_printInfo(&view, &info);
            operation.setJobTitle(Some(&NSString::from_str(title)));
            operation.setShowsPrintPanel(true);
            operation.setShowsProgressPanel(true);
            Ok(if operation.runOperation() {
                Printed::Sent
            } else {
                Printed::Cancelled
            })
        })
    }
}

/// Prints `pdf` through the system's print dialog (see the module's comment): the dialog, then the document's text runs
/// drawn on the printer the person chose. The dialog is modal and runs on this (background) task's thread.
#[cfg(target_os = "windows")]
#[allow(clippy::unused_async)]
pub async fn system(title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    let runs = runs(&pdf);
    drop(pdf);
    win::print(&title, &runs)
}

#[cfg(target_os = "windows")]
mod win {
    use std::mem::{size_of, zeroed};
    use std::ptr::null;

    use windows_sys::Win32::Foundation::GlobalFree;
    use windows_sys::Win32::Graphics::Gdi::{
        CLIP_DEFAULT_PRECIS, CreateFontW, DEFAULT_CHARSET, DEFAULT_QUALITY, DeleteDC, DeleteObject,
        FF_MODERN, FF_SWISS, FIXED_PITCH, FW_BOLD, FW_NORMAL, GetDeviceCaps, HDC, LOGPIXELSX,
        LOGPIXELSY, OUT_DEFAULT_PRECIS, PHYSICALOFFSETX, PHYSICALOFFSETY, SelectObject, SetBkMode,
        SetTextAlign, TA_BASELINE, TA_LEFT, TRANSPARENT, TextOutW, VARIABLE_PITCH,
    };
    use windows_sys::Win32::Storage::Xps::{DOCINFOW, EndDoc, EndPage, StartDocW, StartPage};
    use windows_sys::Win32::UI::Controls::Dialogs::{
        CommDlgExtendedError, PD_NOPAGENUMS, PD_NOSELECTION, PD_RETURNDC, PRINTDLGW, PrintDlgW,
    };
    use zeroize::Zeroizing;

    use super::{PAGE_H, PrintError, Printed, Run};

    /// A device context the dialog answered, deleted when dropped.
    struct Dc(HDC);

    impl Drop for Dc {
        fn drop(&mut self) {
            // SAFETY: the context is the dialog's, created for this print only.
            unsafe { DeleteDC(self.0) };
        }
    }

    fn wide(s: &str) -> Vec<u16> {
        s.encode_utf16().chain(std::iter::once(0)).collect()
    }

    pub(super) fn print(title: &str, runs: &[Run]) -> Result<Printed, PrintError> {
        // SAFETY: a zeroed PRINTDLGW is its documented empty state; only the size and the flags are set.
        let mut dialog: PRINTDLGW = unsafe { zeroed() };
        dialog.lStructSize = size_of::<PRINTDLGW>() as u32;
        dialog.Flags = PD_RETURNDC | PD_NOPAGENUMS | PD_NOSELECTION;
        // SAFETY: the struct is initialised as above and outlives the call.
        let chosen = unsafe { PrintDlgW(&mut dialog) };
        // SAFETY: the dialog's global handles are freed once (null ones are skipped).
        unsafe {
            if !dialog.hDevMode.is_null() {
                GlobalFree(dialog.hDevMode);
            }
            if !dialog.hDevNames.is_null() {
                GlobalFree(dialog.hDevNames);
            }
        }
        if chosen == 0 {
            // SAFETY: no arguments; it reads the dialog's last error on this thread.
            return match unsafe { CommDlgExtendedError() } {
                0 => Ok(Printed::Cancelled),
                code => Err(PrintError::Failed(format!("print dialog error {code:#x}"))),
            };
        }
        if dialog.hDC.is_null() {
            return Err(PrintError::Failed("the dialog answered no printer".into()));
        }
        let dc = Dc(dialog.hDC);
        let name = wide(title);
        let job = DOCINFOW {
            cbSize: size_of::<DOCINFOW>() as i32,
            lpszDocName: name.as_ptr(),
            lpszOutput: null(),
            lpszDatatype: null(),
            fwType: 0,
        };
        // SAFETY: the context is valid until `dc` drops; every pointer handed over outlives its call.
        unsafe {
            if StartDocW(dc.0, &job) <= 0 {
                return Err(PrintError::Failed("the printer refused the job".into()));
            }
            if StartPage(dc.0) <= 0 {
                EndDoc(dc.0);
                return Err(PrintError::Failed("the printer refused the page".into()));
            }
            let (dpi_x, dpi_y) = (
                GetDeviceCaps(dc.0, LOGPIXELSX as i32) as f32,
                GetDeviceCaps(dc.0, LOGPIXELSY as i32) as f32,
            );
            // The context's origin is the printable area's corner, this far into the page.
            let (off_x, off_y) = (
                GetDeviceCaps(dc.0, PHYSICALOFFSETX as i32) as f32,
                GetDeviceCaps(dc.0, PHYSICALOFFSETY as i32) as f32,
            );
            SetBkMode(dc.0, TRANSPARENT as i32);
            SetTextAlign(dc.0, TA_BASELINE | TA_LEFT);
            for run in runs {
                let (face, weight, family) = match run.font {
                    2 => ("Arial", FW_BOLD, VARIABLE_PITCH | FF_SWISS),
                    3 => ("Courier New", FW_NORMAL, FIXED_PITCH | FF_MODERN),
                    _ => ("Arial", FW_NORMAL, VARIABLE_PITCH | FF_SWISS),
                };
                let face = wide(face);
                // A negative height is the em size; points to device pixels at the printer's resolution.
                let font = CreateFontW(
                    -(run.size / 72.0 * dpi_y).round() as i32,
                    0,
                    0,
                    0,
                    weight as i32,
                    0,
                    0,
                    0,
                    u32::from(DEFAULT_CHARSET),
                    u32::from(OUT_DEFAULT_PRECIS),
                    u32::from(CLIP_DEFAULT_PRECIS),
                    u32::from(DEFAULT_QUALITY),
                    u32::from(family),
                    face.as_ptr(),
                );
                let previous = SelectObject(dc.0, font);
                // The PDF's baseline from the page's bottom, in points; the context's from its top, in pixels.
                let x = (run.x / 72.0 * dpi_x - off_x).round() as i32;
                let y = ((PAGE_H - run.y) / 72.0 * dpi_y - off_y).round() as i32;
                let text: Zeroizing<Vec<u16>> = Zeroizing::new(run.text.encode_utf16().collect());
                TextOutW(dc.0, x, y, text.as_ptr(), text.len() as i32);
                SelectObject(dc.0, previous);
                DeleteObject(font);
            }
            EndPage(dc.0);
            EndDoc(dc.0);
        }
        Ok(Printed::Sent)
    }
}

/// Any other platform has no print path.
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
#[allow(clippy::unused_async)]
pub async fn system(_title: String, pdf: Zeroizing<Vec<u8>>) -> Result<Printed, PrintError> {
    drop(pdf);
    Err(PrintError::Unsupported)
}

/// One text run of a [`codes_pdf`] document's content stream (`BT /F<n> <size> Tf <x> <y> Td (<text>) Tj ET`): its face
/// (1 Helvetica, 2 Helvetica-Bold, 3 Courier), size in points and baseline origin in points from the page's lower left,
/// and its text decoded from WinAnsi. What a platform that draws the document itself (Windows) draws.
#[cfg_attr(not(any(target_os = "windows", test)), allow(dead_code))]
pub(crate) struct Run {
    pub font: u8,
    pub size: f32,
    pub x: f32,
    pub y: f32,
    pub text: Zeroizing<String>,
}

/// The text runs of a [`codes_pdf`] document, in order (only the content stream this module writes is read).
#[cfg_attr(not(any(target_os = "windows", test)), allow(dead_code))]
pub(crate) fn runs(pdf: &[u8]) -> Vec<Run> {
    let mut out = Vec::new();
    let Some(start) = find(pdf, b"stream\n").map(|i| i + b"stream\n".len()) else {
        return out;
    };
    let end = find(&pdf[start..], b"\nendstream").map_or(pdf.len(), |i| start + i);
    for line in pdf[start..end].split(|b| *b == b'\n') {
        if let Some(run) = run_of(line) {
            out.push(run);
        }
    }
    out
}

fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

/// `BT /F<n> <size> Tf <x> <y> Td (<text>) Tj ET`.
fn run_of(line: &[u8]) -> Option<Run> {
    let rest = line.strip_prefix(b"BT /F")?;
    let open = rest.iter().position(|b| *b == b'(')?;
    let close = rest.len() - rest.iter().rev().position(|b| *b == b')')? - 1;
    let head = std::str::from_utf8(&rest[..open]).ok()?;
    let mut words = head.split_whitespace();
    let font: u8 = words.next()?.parse().ok()?;
    let size: f32 = words.next()?.parse().ok()?;
    let _tf = words.next()?;
    let x: f32 = words.next()?.parse().ok()?;
    let y: f32 = words.next()?.parse().ok()?;
    Some(Run {
        font,
        size,
        x,
        y,
        text: unescape(&rest[open + 1..close]),
    })
}

/// A PDF literal string's bytes ([`pdf_string`]'s escapes) as text: WinAnsi's codes back to their characters.
fn unescape(s: &[u8]) -> Zeroizing<String> {
    let mut out = Zeroizing::new(String::with_capacity(s.len()));
    let mut i = 0;
    while i < s.len() {
        let mut b = s[i];
        if b == b'\\' && i + 1 < s.len() {
            let octal = &s[i + 1..(i + 4).min(s.len())];
            if octal.len() == 3 && octal.iter().all(|d| (b'0'..=b'7').contains(d)) {
                b = octal
                    .iter()
                    .fold(0u8, |n, d| n.wrapping_mul(8) + (d - b'0'));
                i += 3;
            } else {
                b = s[i + 1];
                i += 1;
            }
        }
        out.push(match b {
            0x85 => '…',
            0x91 => '‘',
            0x92 => '’',
            0x93 => '“',
            0x94 => '”',
            0x96 => '–',
            0x97 => '—',
            b => char::from(b),
        });
        i += 1;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The runs a platform that draws the document itself reads back are the document's own: the title in bold, the
    /// copy, and every code numbered in Courier, top to bottom; escapes and WinAnsi quotes come back as written.
    #[test]
    fn the_runs_read_back_the_document() {
        let doc = codes_pdf(
            "Your (backup) codes",
            "Keep them – they’re yours.",
            "Backup codes",
            &codes(),
        );
        let runs = runs(&doc);
        assert_eq!(runs[0].text.as_str(), "Your (backup) codes");
        assert_eq!(
            (runs[0].font, runs[0].size, runs[0].x),
            (2, TITLE_PT, MARGIN)
        );
        assert_eq!(runs[1].text.as_str(), "Keep them – they’re yours.");
        let coded: Vec<&Run> = runs.iter().filter(|r| r.font == 3).collect();
        assert_eq!(coded.len(), 10);
        for (i, (run, code)) in coded.iter().zip(codes()).enumerate() {
            assert_eq!(run.text.as_str(), format!("{:>2}. {code}", i + 1));
        }
        assert!(
            runs.windows(2).all(|w| w[1].y < w[0].y),
            "each run below the one before"
        );
    }

    fn codes() -> Vec<String> {
        (0..10)
            .map(|i| format!("ABCDE-FGHIJ-KLMNO-P{i:04}"))
            .collect()
    }

    fn pdf() -> Zeroizing<Vec<u8>> {
        codes_pdf(
            "Your backup codes",
            "Each backup code signs you in once if you lose your second factor. Keep them with your recovery key.",
            "Backup codes",
            &codes(),
        )
    }

    /// The document is a PDF whose cross-reference table points at each object it lists, and whose `startxref` points
    /// at that table: what a print backend's parser checks first.
    #[test]
    fn the_pdf_is_well_formed() {
        let bytes = pdf();
        assert!(bytes.starts_with(b"%PDF-1.4\n"));
        assert!(bytes.ends_with(b"%%EOF\n"));
        // Byte offsets: the binary comment's bytes stand in as `?`, one byte each.
        let text = String::from_utf8(
            bytes
                .iter()
                .map(|&b| if b.is_ascii() { b } else { b'?' })
                .collect(),
        )
        .unwrap();
        let start: usize = text
            .rsplit("startxref\n")
            .next()
            .and_then(|t| t.lines().next())
            .and_then(|n| n.parse().ok())
            .expect("startxref");
        assert!(text[start..].starts_with("xref\n0 8\n"));
        let table: Vec<usize> = text[start..]
            .lines()
            .skip(3)
            .take(7)
            .map(|l| l[..10].parse().unwrap())
            .collect();
        for (i, at) in table.iter().enumerate() {
            assert!(
                text[*at..].starts_with(&format!("{} 0 obj\n", i + 1)),
                "object {} at {at}",
                i + 1
            );
        }
        let length: usize = text
            .split("/Length ")
            .nth(1)
            .and_then(|t| t.split(' ').next())
            .and_then(|n| n.parse().ok())
            .unwrap();
        let stream = text.find("stream\n").unwrap() + "stream\n".len();
        assert_eq!(&text[stream + length..stream + length + 10], "\nendstream");
    }

    /// Every code is in the document once, numbered, in the monospaced face; the title and the copy are there.
    #[test]
    fn the_pdf_holds_the_title_the_copy_and_every_code() {
        let text = String::from_utf8_lossy(&pdf()).into_owned();
        assert!(text.contains("(Your backup codes) Tj"));
        assert!(text.contains("(Each backup code signs you in once"));
        assert!(text.contains("/BaseFont /Courier "));
        for (i, code) in codes().iter().enumerate() {
            assert_eq!(
                text.matches(&format!("({:>2}. {code}) Tj", i + 1)).count(),
                1,
                "{code}"
            );
        }
    }

    #[test]
    fn strings_are_escaped_for_pdf() {
        assert_eq!(pdf_string(r"a(b)c\d").as_str(), r"a\(b\)c\\d");
        assert_eq!(pdf_string("You’re in").as_str(), r"You\222re in");
        assert_eq!(pdf_string("é").as_str(), r"\351");
        assert_eq!(pdf_string("⇧").as_str(), "?");
    }

    #[test]
    fn the_copy_wraps_between_words() {
        let lines = wrap("one two three four", 9);
        assert_eq!(lines, ["one two", "three", "four"]);
    }

    #[test]
    fn a_refusal_has_a_code() {
        assert_eq!(PrintError::Unsupported.code(), "E_PRINT_UNSUPPORTED");
        assert_eq!(PrintError::Failed(String::new()).code(), "E_PRINT_FAILED");
    }
}
