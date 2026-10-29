//! Files on this PC, moved by the user and only by the user.
//!
//! The agent never reads the local disk. A local file reaches the agent
//! only when the user picks or drops it, and then only as a copy uploaded to
//! the user's governed workspace through the same files API the web
//! console uses; from there every agent action on it crosses the broker's
//! gates as usual. Going the other way, a workspace file is written to disk
//! only where the user chooses to save it.

use std::io::Write as _;
use std::path::{Path, PathBuf};

use aikonos_client::api::files::MAX_UPLOAD_BYTES;
use gpui_kit::{App, AsyncApp, PathPromptOptions, SharedString};

use crate::prefs::Prefs;

/// A local file read into memory for upload.
pub struct LocalFile {
    pub name: String,
    pub bytes: Vec<u8>,
}

const IMAGE_EXTENSIONS: [&str; 10] = ["png", "jpg", "jpeg", "gif", "webp", "bmp", "tif", "tiff", "heic", "svg"];

pub fn is_image_name(name: &str) -> bool {
    Path::new(name)
        .extension()
        .and_then(|ext| ext.to_str())
        .is_some_and(|ext| IMAGE_EXTENSIONS.iter().any(|known| ext.eq_ignore_ascii_case(known)))
}

/// Where the composer uploads an attachment: images under `references/`,
/// where the vision tool looks for them, everything else at the workspace
/// root (Composer.vue `uploadAttachment`).
pub fn attachment_path(name: &str) -> String {
    if is_image_name(name) {
        format!("references/{name}")
    } else {
        name.to_owned()
    }
}

/// Read a file the user picked, refusing what the server would refuse.
pub fn read_for_upload(path: &Path) -> Result<LocalFile, String> {
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(|| format!("{} has no usable file name.", path.display()))?
        .to_owned();
    let metadata = std::fs::metadata(path).map_err(|err| format!("Can't read {name}: {err}"))?;
    if metadata.is_dir() {
        return Err(format!("{name} is a folder. Pick files instead."));
    }
    if metadata.len() > MAX_UPLOAD_BYTES as u64 {
        return Err(format!(
            "{name} is larger than the {} MB limit.",
            MAX_UPLOAD_BYTES / (1024 * 1024)
        ));
    }
    let bytes = std::fs::read(path).map_err(|err| format!("Can't read {name}: {err}"))?;
    Ok(LocalFile { name, bytes })
}

/// Ask for files to upload. Remembers the folder for next time.
pub async fn pick_files(prompt: &'static str, cx: &mut AsyncApp) -> Vec<PathBuf> {
    let receiver = cx.update(|cx| {
        cx.prompt_for_paths(PathPromptOptions {
            files: true,
            directories: false,
            multiple: true,
            prompt: Some(SharedString::from(prompt)),
        })
    });
    let paths = receiver.await.ok().and_then(Result::ok).flatten().unwrap_or_default();
    if let Some(dir) = paths.first().and_then(|path| path.parent()).map(Path::to_path_buf) {
        cx.update(|cx| Prefs::update(cx, |prefs| prefs.last_open_dir = Some(dir)));
    }
    paths
}

/// Ask where to save `suggested_name`. Starts in the last folder used, else
/// the user's Downloads folder.
pub async fn pick_save_path(suggested_name: &str, cx: &mut AsyncApp) -> Option<PathBuf> {
    let directory = cx
        .update(|cx| Prefs::global(cx).last_save_dir.clone())
        .filter(|dir| dir.is_dir())
        .unwrap_or_else(default_save_dir);
    let suggested_name = safe_file_name(suggested_name);
    let receiver = cx.update(|cx| cx.prompt_for_new_path(&directory, Some(&suggested_name)));
    let path = receiver.await.ok()?.ok()??;
    if let Some(dir) = path.parent().map(Path::to_path_buf) {
        cx.update(|cx| Prefs::update(cx, |prefs| prefs.last_save_dir = Some(dir)));
    }
    Some(path)
}

fn default_save_dir() -> PathBuf {
    std::env::var_os("USERPROFILE")
        .or_else(|| std::env::var_os("HOME"))
        .map(|home| PathBuf::from(home).join("Downloads"))
        .filter(|dir| dir.is_dir())
        .unwrap_or_else(|| PathBuf::from("."))
}

/// A server-supplied name made fit to suggest as a Windows file name: its
/// last path segment, without stream colons, control characters, leading
/// dots or trailing dots and spaces, and never a reserved device name.
pub fn safe_file_name(name: &str) -> String {
    const RESERVED: [&str; 22] = [
        "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1",
        "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
    ];
    let last = name.rsplit(['/', '\\']).next().unwrap_or_default();
    let cleaned: String = last
        .chars()
        .map(|c| if c == ':' || c.is_control() { '_' } else { c })
        .collect();
    let cleaned = cleaned.trim_start_matches(['.', ' ']).trim_end_matches(['.', ' ']);
    let stem = cleaned.split('.').next().unwrap_or_default().trim_end();
    if cleaned.is_empty() {
        "download".to_owned()
    } else if RESERVED.iter().any(|reserved| stem.eq_ignore_ascii_case(reserved)) {
        format!("_{cleaned}")
    } else {
        cleaned.to_owned()
    }
}

/// Write `bytes` to `path` through a temporary file, so an interrupted save
/// never leaves half a file under the user's chosen name. The temporary
/// file has a random name and must not exist yet, so nothing planted in the
/// folder (a symlink included) is followed; the rename then replaces the
/// target in one step. On Windows the file is marked as downloaded from the
/// internet (Mark-of-the-Web), so SmartScreen and Office Protected View
/// treat what an agent or a colleague put in the workspace with suspicion.
pub fn save_bytes(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let name = path.file_name().unwrap_or_default().to_string_lossy();
    let tmp = path.with_file_name(format!(".{name}.{}.aikonos-part", uuid::Uuid::new_v4()));
    let saved = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&tmp)
        .and_then(|mut file| file.write_all(bytes))
        .and_then(|()| {
            // Best effort: FAT drives and some shares have no streams. The
            // rename carries the stream along.
            #[cfg(windows)]
            {
                let mut zone = tmp.clone().into_os_string();
                zone.push(":Zone.Identifier");
                let _ = std::fs::write(zone, "[ZoneTransfer]\r\nZoneId=3\r\n");
            }
            std::fs::rename(&tmp, path)
        });
    if saved.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    saved
}

/// Show a saved file in Explorer.
pub fn reveal(path: &Path, cx: &App) {
    cx.reveal_path(path);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn images_go_to_references() {
        assert_eq!(attachment_path("scan.PNG"), "references/scan.PNG");
        assert_eq!(attachment_path("report.pdf"), "report.pdf");
        assert_eq!(attachment_path("no-extension"), "no-extension");
    }

    #[test]
    fn saving_replaces_atomically() {
        let dir = std::env::temp_dir().join(format!("aikonos-save-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("out.txt");
        save_bytes(&path, b"first").unwrap();
        save_bytes(&path, b"second").unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"second");
        assert_eq!(std::fs::read_dir(&dir).unwrap().count(), 1);
        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn saving_never_follows_a_planted_file() {
        let dir = std::env::temp_dir().join(format!("aikonos-save-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        // The old predictable temporary name, planted ahead of the save.
        let victim = dir.join("victim.txt");
        std::fs::write(&victim, b"keep").unwrap();
        #[cfg(unix)]
        std::os::unix::fs::symlink(&victim, dir.join("report.pdf.aikonos-part")).unwrap();
        save_bytes(&dir.join("report.pdf"), b"report").unwrap();
        assert_eq!(std::fs::read(&victim).unwrap(), b"keep");
        assert_eq!(std::fs::read(dir.join("report.pdf")).unwrap(), b"report");
        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn suggested_names_are_plain_file_names() {
        assert_eq!(safe_file_name("reports/q3.pdf"), "q3.pdf");
        assert_eq!(safe_file_name("..\\..\\x.bat"), "x.bat");
        assert_eq!(safe_file_name("a.txt:evil.exe"), "a.txt_evil.exe");
        assert_eq!(safe_file_name("bad\u{7}name.txt"), "bad_name.txt");
        assert_eq!(safe_file_name(".bashrc"), "bashrc");
        assert_eq!(safe_file_name("notes. . "), "notes");
        assert_eq!(safe_file_name("con.txt"), "_con.txt");
        assert_eq!(safe_file_name("LPT1"), "_LPT1");
        assert_eq!(safe_file_name(".."), "download");
        assert_eq!(safe_file_name("console.log"), "console.log");
    }

    #[test]
    fn oversized_and_folder_uploads_are_refused() {
        let dir = std::env::temp_dir().join(format!("aikonos-upload-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        assert!(read_for_upload(&dir).err().is_some_and(|err| err.contains("folder")));

        let big = dir.join("big.bin");
        std::fs::File::create(&big)
            .unwrap()
            .set_len(MAX_UPLOAD_BYTES as u64 + 1)
            .unwrap();
        assert!(
            read_for_upload(&big)
                .err()
                .is_some_and(|err| err.contains("10 MB limit"))
        );
        std::fs::remove_dir_all(dir).unwrap();
    }
}
