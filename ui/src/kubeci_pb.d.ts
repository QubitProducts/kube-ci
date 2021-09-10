// package: kubeci
// file: kubeci.proto

import * as jspb from "google-protobuf";

export class Repo extends jspb.Message {
  getOrg(): string;
  setOrg(value: string): void;

  getRepo(): string;
  setRepo(value: string): void;

  getBranch(): string;
  setBranch(value: string): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): Repo.AsObject;
  static toObject(includeInstance: boolean, msg: Repo): Repo.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: Repo, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): Repo;
  static deserializeBinaryFromReader(message: Repo, reader: jspb.BinaryReader): Repo;
}

export namespace Repo {
  export type AsObject = {
    org: string,
    repo: string,
    branch: string,
  }
}

export class ListTemplatesRequest extends jspb.Message {
  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ListTemplatesRequest.AsObject;
  static toObject(includeInstance: boolean, msg: ListTemplatesRequest): ListTemplatesRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ListTemplatesRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ListTemplatesRequest;
  static deserializeBinaryFromReader(message: ListTemplatesRequest, reader: jspb.BinaryReader): ListTemplatesRequest;
}

export namespace ListTemplatesRequest {
  export type AsObject = {
  }
}

export class ListTemplatesResponse extends jspb.Message {
  clearNameList(): void;
  getNameList(): Array<string>;
  setNameList(value: Array<string>): void;
  addName(value: string, index?: number): string;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ListTemplatesResponse.AsObject;
  static toObject(includeInstance: boolean, msg: ListTemplatesResponse): ListTemplatesResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ListTemplatesResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ListTemplatesResponse;
  static deserializeBinaryFromReader(message: ListTemplatesResponse, reader: jspb.BinaryReader): ListTemplatesResponse;
}

export namespace ListTemplatesResponse {
  export type AsObject = {
    nameList: Array<string>,
  }
}

export class SetupRepoRequest extends jspb.Message {
  hasRepo(): boolean;
  clearRepo(): void;
  getRepo(): Repo | undefined;
  setRepo(value?: Repo): void;

  getTemplate(): string;
  setTemplate(value: string): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): SetupRepoRequest.AsObject;
  static toObject(includeInstance: boolean, msg: SetupRepoRequest): SetupRepoRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: SetupRepoRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): SetupRepoRequest;
  static deserializeBinaryFromReader(message: SetupRepoRequest, reader: jspb.BinaryReader): SetupRepoRequest;
}

export namespace SetupRepoRequest {
  export type AsObject = {
    repo?: Repo.AsObject,
    template: string,
  }
}

export class SetupRepoResponse extends jspb.Message {
  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): SetupRepoResponse.AsObject;
  static toObject(includeInstance: boolean, msg: SetupRepoResponse): SetupRepoResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: SetupRepoResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): SetupRepoResponse;
  static deserializeBinaryFromReader(message: SetupRepoResponse, reader: jspb.BinaryReader): SetupRepoResponse;
}

export namespace SetupRepoResponse {
  export type AsObject = {
  }
}

export class LaunchWorkflowRequest extends jspb.Message {
  hasRepo(): boolean;
  clearRepo(): void;
  getRepo(): Repo | undefined;
  setRepo(value?: Repo): void;

  getCommit(): string;
  setCommit(value: string): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): LaunchWorkflowRequest.AsObject;
  static toObject(includeInstance: boolean, msg: LaunchWorkflowRequest): LaunchWorkflowRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: LaunchWorkflowRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): LaunchWorkflowRequest;
  static deserializeBinaryFromReader(message: LaunchWorkflowRequest, reader: jspb.BinaryReader): LaunchWorkflowRequest;
}

export namespace LaunchWorkflowRequest {
  export type AsObject = {
    repo?: Repo.AsObject,
    commit: string,
  }
}

export class LaunchWorkflowResponse extends jspb.Message {
  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): LaunchWorkflowResponse.AsObject;
  static toObject(includeInstance: boolean, msg: LaunchWorkflowResponse): LaunchWorkflowResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: LaunchWorkflowResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): LaunchWorkflowResponse;
  static deserializeBinaryFromReader(message: LaunchWorkflowResponse, reader: jspb.BinaryReader): LaunchWorkflowResponse;
}

export namespace LaunchWorkflowResponse {
  export type AsObject = {
  }
}

