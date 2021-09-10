// package: kubeci
// file: kubeci.proto

import * as kubeci_pb from "./kubeci_pb";
import {grpc} from "@improbable-eng/grpc-web";

type KubeCIListTemplate = {
  readonly methodName: string;
  readonly service: typeof KubeCI;
  readonly requestStream: false;
  readonly responseStream: false;
  readonly requestType: typeof kubeci_pb.ListTemplatesRequest;
  readonly responseType: typeof kubeci_pb.ListTemplatesResponse;
};

type KubeCISetupRepo = {
  readonly methodName: string;
  readonly service: typeof KubeCI;
  readonly requestStream: false;
  readonly responseStream: false;
  readonly requestType: typeof kubeci_pb.SetupRepoRequest;
  readonly responseType: typeof kubeci_pb.SetupRepoResponse;
};

export class KubeCI {
  static readonly serviceName: string;
  static readonly ListTemplate: KubeCIListTemplate;
  static readonly SetupRepo: KubeCISetupRepo;
}

export type ServiceError = { message: string, code: number; metadata: grpc.Metadata }
export type Status = { details: string, code: number; metadata: grpc.Metadata }

interface UnaryResponse {
  cancel(): void;
}
interface ResponseStream<T> {
  cancel(): void;
  on(type: 'data', handler: (message: T) => void): ResponseStream<T>;
  on(type: 'end', handler: (status?: Status) => void): ResponseStream<T>;
  on(type: 'status', handler: (status: Status) => void): ResponseStream<T>;
}
interface RequestStream<T> {
  write(message: T): RequestStream<T>;
  end(): void;
  cancel(): void;
  on(type: 'end', handler: (status?: Status) => void): RequestStream<T>;
  on(type: 'status', handler: (status: Status) => void): RequestStream<T>;
}
interface BidirectionalStream<ReqT, ResT> {
  write(message: ReqT): BidirectionalStream<ReqT, ResT>;
  end(): void;
  cancel(): void;
  on(type: 'data', handler: (message: ResT) => void): BidirectionalStream<ReqT, ResT>;
  on(type: 'end', handler: (status?: Status) => void): BidirectionalStream<ReqT, ResT>;
  on(type: 'status', handler: (status: Status) => void): BidirectionalStream<ReqT, ResT>;
}

export class KubeCIClient {
  readonly serviceHost: string;

  constructor(serviceHost: string, options?: grpc.RpcOptions);
  listTemplate(
    requestMessage: kubeci_pb.ListTemplatesRequest,
    metadata: grpc.Metadata,
    callback: (error: ServiceError|null, responseMessage: kubeci_pb.ListTemplatesResponse|null) => void
  ): UnaryResponse;
  listTemplate(
    requestMessage: kubeci_pb.ListTemplatesRequest,
    callback: (error: ServiceError|null, responseMessage: kubeci_pb.ListTemplatesResponse|null) => void
  ): UnaryResponse;
  setupRepo(
    requestMessage: kubeci_pb.SetupRepoRequest,
    metadata: grpc.Metadata,
    callback: (error: ServiceError|null, responseMessage: kubeci_pb.SetupRepoResponse|null) => void
  ): UnaryResponse;
  setupRepo(
    requestMessage: kubeci_pb.SetupRepoRequest,
    callback: (error: ServiceError|null, responseMessage: kubeci_pb.SetupRepoResponse|null) => void
  ): UnaryResponse;
}

