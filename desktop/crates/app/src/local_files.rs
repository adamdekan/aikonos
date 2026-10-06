//! Files on this PC, moved by the user and only by the user.
//!
//! The agent never reads the local disk. A local file reaches the agent
//! only when the user picks or drops it, and then only as a copy uploaded to
//! the user's governed workspace through the same files API the web
//! console uses; from there every agent action on it crosses the broker's
//! gates as usual. Going the other way, a workspace file is written to disk
//! only where the user chooses to save it.

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
    let receiver = cx.update(|cx| cx.prompt_for_new_path(&directory, Some(suggested_name)));
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

/// Write `bytes` to `path` through a temporary file, so an interrupted save
/// never leaves half a file under the user's chosen name.
pub fn save_bytes(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let tmp = path.with_extension(format!(
        "{}aikonos-part",
        path.extension()
            .and_then(|ext| ext.to_str())
            .map(|ext| format!("{ext}."))
            .unwrap_or_default()
    ));
    std::fs::write(&tmp, bytes)?;
    if path.exists() {
        std::fs::remove_file(path)?;
    }
    std::fs::rename(&tmp, path)
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
