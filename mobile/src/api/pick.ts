import * as DocumentPicker from 'expo-document-picker';
import * as ImagePicker from 'expo-image-picker';
import { Platform } from 'react-native';

/**
 * 手机上的三个入口：相册、拍照、文件。
 *
 * 这里只负责「挑到一个文件」，不碰网络也不碰会话 —— 传上去、挂到消息上是 state 层的事
 * （`mobile.ts` 的 uploadFile / send）。三个入口共用一个形状，因为上传那条路只认一种东西：
 * 一个 uri（原生）或一个 File（web），加一个名字。
 */
export interface PickedFile {
  uri: string;
  /** 发送方给的名字。宿主要用它给文件命名，手机也要在 chip 上把它显示回来 */
  name: string;
  mimeType: string;
  /** 选择器报的大小（可能没有）；用来在传之前拦掉一个明显太大的文件 */
  size?: number;
  /** web：选择器直接给一个 File，FormData 收它，不必再去读 blob */
  file?: File;
}

const isWeb = Platform.OS === 'web';

/**
 * JPEG 而不留原图：iPhone 的照片是 HEIC，而模型只认 png / jpg / gif / webp —— 一张 HEIC 传
 * 上去是一段谁也看不见的二进制。`compatible` 让系统把它转成 JPEG，`quality` 顺手把它压在
 * 模型那条 5 MB 的线以下（原图动辄 4–8 MB，超了只会被服务端退回来）。
 */
const PHOTO_QUALITY = 0.8;

function photoName(uri: string): string {
  const base = uri.split('/').pop();
  return base && base.includes('.') ? base : `photo-${Date.now()}.jpg`;
}

/**
 * 从相册选一张，或直接拍一张。取消、没给权限都回 null —— 那是用户的选择，不是错误。
 *
 * 拍照要相机权限：扫码那里已经申请过同一个权限（app.json 的 expo-camera 插件），这里是
 * 第二个使用方，所以也自己问一次，避免用户从没扫过码时弹不出系统对话框。
 */
export async function pickPhoto(source: 'library' | 'camera'): Promise<PickedFile | null> {
  if (source === 'camera') {
    const perm = await ImagePicker.requestCameraPermissionsAsync();
    if (!perm.granted) return null;
  }
  const opts: ImagePicker.ImagePickerOptions = {
    mediaTypes: ['images'],
    quality: PHOTO_QUALITY,
    allowsMultipleSelection: false,
    // 只在 iOS 上有意义：把 HEIC 转成 JPEG（见 PHOTO_QUALITY 上面那段）
    preferredAssetRepresentationMode: ImagePicker.UIImagePickerPreferredAssetRepresentationMode.Compatible,
  };
  const res =
    source === 'camera'
      ? await ImagePicker.launchCameraAsync(opts)
      : await ImagePicker.launchImageLibraryAsync(opts);
  if (res.canceled || !res.assets?.length) return null;
  const a = res.assets[0];
  return {
    uri: a.uri,
    name: a.fileName || photoName(a.uri),
    // 相机和相册都给 jpg；web 上可能还是原图的类型
    mimeType: a.mimeType || 'image/jpeg',
    size: a.fileSize,
    file: isWeb ? (a.file ?? undefined) : undefined,
  };
}

/** 从系统的文件选择器里挑一个文件（PDF、日志、代码、压缩包…）。取消回 null。 */
export async function pickDocument(): Promise<PickedFile | null> {
  const res = await DocumentPicker.getDocumentAsync({
    // 选中的文件先拷进 App 的缓存目录：有的来源（iCloud、其它 App 的 provider）给的 uri
    // 只在那一刻可读，直接拿去上传会失败。
    copyToCacheDirectory: true,
    multiple: false,
  });
  if (res.canceled || !res.assets?.length) return null;
  const a = res.assets[0];
  return {
    uri: a.uri,
    name: a.name || photoName(a.uri),
    mimeType: a.mimeType || '',
    size: a.size,
    file: isWeb ? ((a as { file?: File }).file ?? undefined) : undefined,
  };
}
