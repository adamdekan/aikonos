//! Embeds the app icon and version details in aikonos.exe: Explorer and the
//! taskbar show the icon, Task Manager the description. GPUI gives the window
//! the executable's icon resource 1.

use std::path::PathBuf;

fn main() {
    println!("cargo:rerun-if-changed=resources/aikonos.ico");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() != Ok("windows") {
        return;
    }
    let version = std::env::var("CARGO_PKG_VERSION").expect("cargo sets the version");
    let mut numbers = version
        .split(['.', '-', '+'])
        .map(|part| part.parse::<u16>().unwrap_or(0));
    let [major, minor, patch] = [(); 3].map(|_| numbers.next().unwrap_or(0));

    let manifest_dir = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").expect("cargo sets the manifest dir"));
    let icon = manifest_dir.join("resources").join("aikonos.ico");
    let icon = icon.display().to_string().replace('\\', "\\\\");
    let script = format!(
        r#"1 ICON "{icon}"

1 VERSIONINFO
FILEVERSION {major},{minor},{patch},0
PRODUCTVERSION {major},{minor},{patch},0
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904B0"
    BEGIN
      VALUE "FileDescription", "Aikonos"
      VALUE "FileVersion", "{version}"
      VALUE "InternalName", "aikonos"
      VALUE "OriginalFilename", "aikonos.exe"
      VALUE "ProductName", "Aikonos for Windows"
      VALUE "ProductVersion", "{version}"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
"#
    );
    let out_dir = PathBuf::from(std::env::var("OUT_DIR").expect("cargo sets OUT_DIR"));
    let rc = out_dir.join("aikonos.rc");
    std::fs::write(&rc, script).expect("write the resource script");
    embed_resource::compile(&rc, embed_resource::NONE)
        .manifest_optional()
        .expect("compile the resource script");
}
